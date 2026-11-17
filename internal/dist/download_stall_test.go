package dist

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tuneRetries shortens every wait for one test and restores the defaults afterwards.
func tuneRetries(t *testing.T, stall time.Duration) {
	t.Helper()
	oldStall, oldBase, oldMax := stallTimeout, backoffBase, backoffMax
	stallTimeout, backoffBase, backoffMax = stall, time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { stallTimeout, backoffBase, backoffMax = oldStall, oldBase, oldMax })
}

// retryLog collects the progress labels that announce a retry.
type retryLog struct {
	mu    sync.Mutex
	lines []string
}

func (r *retryLog) progress(done, total int64, label string) {
	if strings.HasPrefix(label, "retry ") {
		r.mu.Lock()
		r.lines = append(r.lines, label)
		r.mu.Unlock()
	}
}

// Every transient status is retried until the download succeeds (PZL-9, item 7).
func TestDownloadRetriesEveryTransientStatus(t *testing.T) {
	tuneRetries(t, time.Minute)
	body := []byte("transient payload")
	for _, code := range []int{408, 425, 429, 500, 502, 503, 504} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&hits, 1) == 1 {
					http.Error(w, "try later", code)
					return
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "layer.tar.gz")
			layer := Layer{URL: srv.URL + "/layer.tar.gz", Bytes: int64(len(body)), SHA256: sha(body)}
			if err := Download(context.Background(), layer, dest, nil); err != nil {
				t.Fatalf("HTTP %d must be retried: %v", code, err)
			}
			if n := atomic.LoadInt32(&hits); n != 2 {
				t.Fatalf("HTTP %d: %d requests, want 2", code, n)
			}
		})
	}
}

// Permanent client errors fail at once, and the error names the status (PZL-9, item 7).
func TestDownloadFailsFastOnPermanentStatus(t *testing.T) {
	tuneRetries(t, time.Minute)
	for _, code := range []int{400, 401, 403, 404, 410} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				http.Error(w, "no", code)
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "layer.tar.gz")
			layer := Layer{URL: srv.URL + "/layer.tar.gz", Bytes: 4, SHA256: sha([]byte("gone"))}
			err := Download(context.Background(), layer, dest, nil)
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) {
				t.Fatalf("want an error naming HTTP %d, got %v", code, err)
			}
			if n := atomic.LoadInt32(&hits); n != 1 {
				t.Fatalf("HTTP %d requested %d times, want 1", code, n)
			}
		})
	}
}

// A body that stops for stallTimeout is abandoned, and the download resumes with Range and If-Match
// (PZL-9, item 7 and the stall watchdog).
func TestDownloadResumesAfterIdleStall(t *testing.T) {
	tuneRetries(t, 150*time.Millisecond)
	body := bytes.Repeat([]byte("abcdefgh"), 4096)
	half := len(body) / 2
	var hits int32
	var mu sync.Mutex
	var gotRange, gotIfMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body[:half])
			w.(http.Flusher).Flush()
			<-r.Context().Done() // stall until the client gives up
			return
		}
		mu.Lock()
		gotRange, gotIfMatch = r.Header.Get("Range"), r.Header.Get("If-Match")
		mu.Unlock()
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[half:])
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "layer.tar.gz")
	layer := Layer{URL: srv.URL + "/layer.tar.gz", Bytes: int64(len(body)), SHA256: sha(body)}
	var rl retryLog
	if err := Download(context.Background(), layer, dest, rl.progress); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotRange != fmt.Sprintf("bytes=%d-", half) || gotIfMatch != `"v1"` {
		t.Fatalf("resume sent Range %q and If-Match %q", gotRange, gotIfMatch)
	}
	if len(rl.lines) != 1 {
		t.Fatalf("%d retry lines for one stall, want 1", len(rl.lines))
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Fatal("content differs after the resume")
	}
}

// If the object changed, the server answers 412 and the download starts again from byte zero.
func TestDownloadRestartsFromZeroWhenObjectChanged(t *testing.T) {
	tuneRetries(t, time.Minute)
	body := []byte("the new object, complete and consistent")
	dest := filepath.Join(t.TempDir(), "layer.tar.gz")
	if err := os.WriteFile(dest+".part", []byte("old obj"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".part.etag", []byte(`"old"`), 0o644); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		w.Header().Set("ETag", `"new"`)
		if ifm := r.Header.Get("If-Match"); ifm != "" && ifm != `"new"` {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	layer := Layer{URL: srv.URL + "/layer.tar.gz", Bytes: int64(len(body)), SHA256: sha(body)}
	if err := Download(context.Background(), layer, dest, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Fatal("content differs after the restart")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 2 || ranges[0] != "bytes=7-" || ranges[1] != "" {
		t.Fatalf("requests sent Range %q, want [bytes=7- empty]", ranges)
	}
}

// A connection that closes before Content-Length is resumed, not reported as a finished download.
func TestDownloadResumesAfterEarlyClose(t *testing.T) {
	tuneRetries(t, time.Minute)
	body := bytes.Repeat([]byte("0123456789"), 2000)
	half := len(body) / 2
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: \"v1\"\r\nConnection: close\r\n\r\n", len(body))
			_, _ = buf.Write(body[:half])
			_ = buf.Flush()
			_ = conn.Close()
			return
		}
		if got := r.Header.Get("Range"); got != fmt.Sprintf("bytes=%d-", half) {
			t.Errorf("resume sent Range %q", got)
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[half:])
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "layer.tar.gz")
	layer := Layer{URL: srv.URL + "/layer.tar.gz", Bytes: int64(len(body)), SHA256: sha(body)}
	if err := Download(context.Background(), layer, dest, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Fatal("content differs after the resume")
	}
}

// A caller's deadline ends the retries, even when the server never recovers.
func TestDownloadStopsRetryingWhenContextEnds(t *testing.T) {
	tuneRetries(t, time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	body := []byte("never arrives")
	dest := filepath.Join(t.TempDir(), "layer.tar.gz")
	layer := Layer{URL: srv.URL + "/layer.tar.gz", Bytes: int64(len(body)), SHA256: sha(body)}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Download(ctx, layer, dest, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("retries kept going for %s after the deadline", elapsed)
	}
}

// Each retry logs exactly one progress line, labelled with its number.
func TestEachRetryLogsOneLine(t *testing.T) {
	tuneRetries(t, time.Minute)
	body := []byte("three strikes")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 3 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "layer.tar.gz")
	layer := Layer{URL: srv.URL + "/layer.tar.gz", Bytes: int64(len(body)), SHA256: sha(body)}
	var rl retryLog
	if err := Download(context.Background(), layer, dest, rl.progress); err != nil {
		t.Fatal(err)
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if len(rl.lines) != 3 {
		t.Fatalf("%d retry lines for three failures", len(rl.lines))
	}
	for i, line := range rl.lines {
		if !strings.HasPrefix(line, fmt.Sprintf("retry %d: ", i+1)) {
			t.Fatalf("line %d is %q", i, line)
		}
	}
}

// The wait doubles from one second to a 60-second cap, and jitter keeps it within [d/2, d].
func TestBackoffDoublesCapsAndJitters(t *testing.T) {
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 60 * time.Second, 60 * time.Second}
	d := time.Second
	for i, w := range want {
		d = nextBackoff(d)
		if d != w {
			t.Fatalf("step %d: %v, want %v", i+1, d, w)
		}
	}
	for i := 0; i < 200; i++ {
		if j := jitter(time.Second); j < 500*time.Millisecond || j > time.Second {
			t.Fatalf("jitter(1s) = %v", j)
		}
	}
}
