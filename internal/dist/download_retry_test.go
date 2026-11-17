package dist

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A transient 503 must be retried, not reported as a failed download (PZL-3 finding 13).
func TestDownloadRetriesTransient5xx(t *testing.T) {
	tuneRetries(t, time.Minute)
	body := []byte("layer payload")
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "base.tar.gz")
	layer := Layer{URL: srv.URL + "/base.tar.gz", Bytes: int64(len(body)), SHA256: sha(body)}
	if err := Download(context.Background(), layer, dest, nil); err != nil {
		t.Fatalf("a 503 must be retried: %v (requests=%d)", err, atomic.LoadInt32(&hits))
	}
}

// A 404 is a permanent error: exactly one request and a clear failure.
func TestDownloadFailsFastOn404(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "base.tar.gz")
	layer := Layer{URL: srv.URL + "/missing", Bytes: 4, SHA256: sha([]byte("gone"))}
	if err := Download(context.Background(), layer, dest, nil); err == nil {
		t.Fatal("404 must fail")
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("404 was requested %d times, want 1", n)
	}
}

// A cached archive with the right size and a stale marker must be re-hashed, not trusted
// (PZL-3 finding 13, observation D2).
func TestDownloadRejectsSameSizeCorruptCache(t *testing.T) {
	good := []byte("0123456789")
	bad := []byte("abcdefghij")
	dest := filepath.Join(t.TempDir(), "base.tar.gz")
	if err := os.WriteFile(dest, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".ok", []byte(sha(good)), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(good)
	}))
	defer srv.Close()
	layer := Layer{URL: srv.URL + "/base.tar.gz", Bytes: int64(len(good)), SHA256: sha(good)}
	if err := Download(context.Background(), layer, dest, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, good) {
		t.Fatal("a same-size corrupt archive was accepted from the cache")
	}
}
