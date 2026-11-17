package dist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Retry and stall settings. They are variables so that tests can shorten them.
var (
	// stallTimeout abandons a request that produces no body bytes for this long. The download
	// then resumes from the .part file with a Range request.
	stallTimeout = 60 * time.Second
	// backoffBase and backoffMax bound the wait between attempts. The wait starts at backoffBase,
	// doubles up to backoffMax and is jittered. It starts again at backoffBase after any attempt
	// that received bytes.
	backoffBase = time.Second
	backoffMax  = 60 * time.Second
)

// retryable marks a failed attempt that a later attempt may fix. Every other failure ends the
// download at once.
type retryable struct {
	reason  string // one line for the progress log
	err     error
	restart bool  // retry at once from byte zero: the object changed
	done    int64 // bytes on disk when the attempt failed
	total   int64 // expected size, 0 when unknown
}

func (r *retryable) Error() string { return r.reason }
func (r *retryable) Unwrap() error { return r.err }

// downloadClient is shared so that retries reuse connections.
var downloadClient = httpClient()

// errDigestMismatch and errSizeMismatch mark a finished download whose bytes are not the object the
// manifest names. The file is already removed, so a caller may download the object again.
var (
	errDigestMismatch = errors.New("sha256 mismatch")
	errSizeMismatch   = errors.New("size mismatch")
)

// Download fetches layer.URL to dest and verifies the manifest SHA-256 before the file is used.
// It keeps trying, with capped jittered backoff, until the download succeeds or ctx ends.
// Retried: transport errors, EOF before the expected size, a body that stalls for stallTimeout,
// HTTP 408, 425, 429 and every 5xx. Not retried: every other 4xx, which fails at once with the
// status in the error. Each retry logs one progress line labelled "retry N:". A resume sends a
// Range request guarded by If-Match with the ETag of the first response.
func Download(ctx context.Context, layer Layer, dest string, progress Progress) error {
	if layer.URL == "" {
		return fmt.Errorf("empty url")
	}
	if layer.SHA256 == "" {
		return fmt.Errorf("manifest has no sha256 for %s", layer.URL)
	}
	if done, _ := verified(dest, layer); done {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	delay := backoffBase
	for attempt := 1; ; attempt++ {
		progressed, err := downloadOnce(ctx, layer, dest, progress)
		if err == nil {
			return nil
		}
		var r *retryable
		if !errors.As(err, &r) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if progressed {
			delay = backoffBase
		}
		wait := jitter(delay)
		if r.restart {
			wait = 0
		}
		notifyRetry(progress, attempt, r, wait)
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		delay = nextBackoff(delay)
	}
}

// notifyRetry reports one retry as one progress line.
func notifyRetry(progress Progress, attempt int, r *retryable, wait time.Duration) {
	if progress == nil {
		return
	}
	label := fmt.Sprintf("retry %d: %s", attempt, r.reason)
	if wait > 0 {
		label += fmt.Sprintf("; next try in %s", wait.Round(100*time.Millisecond))
	}
	progress(r.done, r.total, label)
}

// nextBackoff doubles a wait, capped at backoffMax.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

// jitter spreads a wait over [d/2, d], so that clients that failed together do not retry together.
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(d-half)+1))
}

// downloadOnce makes one attempt. It reports whether any body bytes arrived. Failures worth
// another attempt are returned as *retryable.
func downloadOnce(ctx context.Context, layer Layer, dest string, progress Progress) (bool, error) {
	part := dest + ".part"
	etagPath := part + ".etag"

	offset := int64(0)
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
	}
	// A previous attempt wrote the whole body but died before verifying it.
	if layer.Bytes > 0 && offset == layer.Bytes {
		if err := verifyAndSeal(part, dest, layer); err == nil {
			return false, nil
		}
		offset = 0 // verifyAndSeal removed the bad part; start again
	}
	etag := ""
	if offset > 0 {
		etag = readETag(etagPath)
		if etag == "" {
			// Without the validator from the first response, the object may have changed.
			_ = os.Remove(part)
			offset = 0
		}
	}

	// The watchdog cancels this request when no body bytes arrive for stallTimeout.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(stallTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, layer.URL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		req.Header.Set("If-Match", etag)
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		reason := "request failed: " + err.Error()
		if stalled.Load() {
			reason = fmt.Sprintf("no response for %s", stallTimeout)
		}
		return false, &retryable{reason: reason + "; retrying", err: err, done: offset, total: layer.Bytes}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		offset = 0 // a whole body: a fresh download, or a server that ignored Range
	case resp.StatusCode == http.StatusPartialContent:
		if cr := resp.Header.Get("Content-Range"); cr != "" && offset > 0 && !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", offset)) {
			_ = os.Remove(part)
			_ = os.Remove(etagPath)
			return false, &retryable{reason: "server resumed at the wrong offset; restarting from byte 0", restart: true, total: layer.Bytes}
		}
	case resp.StatusCode == http.StatusPreconditionFailed && offset > 0:
		// If-Match failed: the object changed since the first response. Start over.
		_ = os.Remove(part)
		_ = os.Remove(etagPath)
		return false, &retryable{reason: "object changed on the server; restarting from byte 0", restart: true, total: layer.Bytes}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		_ = os.Remove(part)
		_ = os.Remove(etagPath)
		return false, fmt.Errorf("range not satisfiable for %s; discarded the partial download, run again", layer.URL)
	case transientStatus(resp.StatusCode):
		return false, &retryable{reason: "HTTP " + resp.Status + "; retrying", done: offset, total: layer.Bytes}
	default:
		return false, fmt.Errorf("download %s failed: HTTP %s (not retried)", layer.URL, resp.Status)
	}

	// Keep the validator of this object for the resumes that follow.
	if et := strongETag(resp.Header.Get("ETag")); et != "" {
		if err := os.WriteFile(etagPath, []byte(et), 0o644); err != nil {
			return false, err
		}
	} else if resp.StatusCode == http.StatusOK {
		_ = os.Remove(etagPath)
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	if offset == 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return false, err
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return false, err
	}

	expected := layer.Bytes
	if expected <= 0 && resp.ContentLength >= 0 {
		expected = offset + resp.ContentLength
	}
	done := offset
	progressed := false
	last := time.Now()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return progressed, werr
			}
			done += int64(n)
			progressed = true
			watchdog.Reset(stallTimeout)
			if progress != nil && time.Since(last) > 120*time.Millisecond {
				progress(done, expected, "downloading")
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			if ctx.Err() != nil {
				return progressed, ctx.Err()
			}
			reason := fmt.Sprintf("connection interrupted at %d bytes (%v); resuming", done, rerr)
			if stalled.Load() {
				reason = fmt.Sprintf("no data for %s at %d bytes; resuming", stallTimeout, done)
			}
			return progressed, &retryable{reason: reason, err: rerr, done: done, total: expected}
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return progressed, err
	}
	if err := f.Close(); err != nil {
		return progressed, err
	}
	if progress != nil {
		progress(done, expected, "downloading")
	}
	if expected > 0 && done < expected {
		return progressed, &retryable{
			reason: fmt.Sprintf("connection closed at %d of %d bytes; resuming", done, expected),
			done:   done, total: expected,
		}
	}
	if expected > 0 && done != expected {
		_ = os.Remove(part)
		_ = os.Remove(etagPath)
		return progressed, fmt.Errorf("%w for %s: got %d, want %d", errSizeMismatch, layer.URL, done, expected)
	}
	return progressed, verifyAndSeal(part, dest, layer)
}

// verified reports whether dest is the archive the manifest names. The file is re-hashed: a
// cached archive is trusted for the bytes it holds, never for its size or a marker. A file that
// fails is removed, so the next attempt downloads it again.
func verified(dest string, layer Layer) (bool, error) {
	st, err := os.Stat(dest)
	if err != nil {
		return false, err
	}
	if layer.Bytes > 0 && st.Size() != layer.Bytes {
		_ = os.Remove(dest)
		return false, fmt.Errorf("size mismatch")
	}
	sum, err := sha256File(dest)
	if err != nil {
		return false, err
	}
	if sum != layer.SHA256 {
		_ = os.Remove(dest)
		return false, fmt.Errorf("sha256 mismatch")
	}
	return true, nil
}

// verifyAndSeal flushes the part file to disk, checks its SHA-256 and renames it to dest. A
// mismatch removes the part file and its validator, since neither can be resumed.
func verifyAndSeal(part, dest string, layer Layer) error {
	if err := syncFile(part); err != nil {
		return err
	}
	sum, err := sha256File(part)
	if err != nil {
		return err
	}
	if sum != layer.SHA256 {
		_ = os.Remove(part)
		_ = os.Remove(part + ".etag")
		return fmt.Errorf("%w for %s: got %s want %s", errDigestMismatch, layer.URL, sum, layer.SHA256)
	}
	if err := os.Rename(part, dest); err != nil {
		// Windows cannot replace an existing file with Rename.
		if rmErr := os.Remove(dest); rmErr != nil {
			return err
		}
		if err := os.Rename(part, dest); err != nil {
			return err
		}
	}
	_ = os.Remove(part + ".etag")
	return nil
}

// syncFile flushes a file's contents to disk, so that a crash after the rename cannot leave an
// empty or partial file under the final name.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readETag returns the validator stored with a partial download, or "".
func readETag(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// strongETag returns an ETag usable in If-Match. Weak validators (W/) are not, so they are not
// kept.
func strongETag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "W/") {
		return ""
	}
	return v
}

// transientStatus lists the statuses that a later attempt can fix: 408, 425, 429 and every 5xx.
func transientStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooEarly ||
		code == http.StatusTooManyRequests || (code >= 500 && code <= 599)
}
