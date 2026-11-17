package pz

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

var errTransientTest = errors.New("test: sharing violation")

// stubRename replaces the rename primitive for one test. Errors that wrap
// errTransientTest count as transient, and retries wait no time. It returns a
// pointer to the number of rename calls made.
func stubRename(t *testing.T, fn func(oldpath, newpath string) error) *int {
	t.Helper()
	calls := 0
	prevRename, prevRetry, prevBackoff := renameFile, retryableRenameErr, renameBackoff
	renameFile = func(oldpath, newpath string) error {
		calls++
		return fn(oldpath, newpath)
	}
	retryableRenameErr = func(err error) bool { return errors.Is(err, errTransientTest) }
	renameBackoff = 0
	t.Cleanup(func() {
		renameFile, retryableRenameErr, renameBackoff = prevRename, prevRetry, prevBackoff
	})
	return &calls
}

func TestReplaceFileRetriesTransientSharingViolation(t *testing.T) {
	failures := 2
	calls := stubRename(t, func(string, string) error {
		if failures > 0 {
			failures--
			return errTransientTest
		}
		return nil
	})
	if err := replaceFile("tmp", "dst"); err != nil {
		t.Fatalf("expected success after transient failures: %v", err)
	}
	if *calls != 3 {
		t.Fatalf("rename attempts = %d, want 3", *calls)
	}
}

func TestReplaceFileRetriesAreBounded(t *testing.T) {
	calls := stubRename(t, func(string, string) error { return errTransientTest })
	err := replaceFile("tmp", "dst")
	if !errors.Is(err, errTransientTest) {
		t.Fatalf("error should wrap the cause, got %v", err)
	}
	if *calls != renameAttempts {
		t.Fatalf("rename attempts = %d, want %d", *calls, renameAttempts)
	}
}

func TestReplaceFileDoesNotRetryOtherErrors(t *testing.T) {
	calls := stubRename(t, func(string, string) error { return os.ErrPermission })
	if err := replaceFile("tmp", "dst"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("err = %v, want the permission error", err)
	}
	if *calls != 1 {
		t.Fatalf("rename attempts = %d, want 1", *calls)
	}
}

func TestReplaceFileReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "default.txt")
	tmp := filepath.Join(dir, "default.txt.tmp-1")
	if err := os.WriteFile(dst, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(tmp, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new\n" {
		t.Fatalf("destination = %q", b)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestReplaceFileKeepsDestinationWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "default.txt")
	tmp := filepath.Join(dir, "default.txt.tmp-1")
	if err := os.WriteFile(dst, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubRename(t, func(string, string) error { return os.ErrPermission })
	if err := replaceFile(tmp, dst); err == nil {
		t.Fatal("expected the failure to surface")
	}
	if b, _ := os.ReadFile(dst); string(b) != "old\n" {
		t.Fatalf("destination changed on a failed replace: %q", b)
	}
}

func TestWriteFileAtomicKeepsExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "ProjectZomboid64.json")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content = %q", b)
	}
}

func TestWriteFileAtomicNewFileIs0644(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	path := filepath.Join(t.TempDir(), "fresh.txt")
	if err := writeFileAtomic(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("mode = %v, want 0644", st.Mode().Perm())
	}
}

func TestReadIfExistsReportsUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	if _, existed, err := readIfExists(dir); err == nil || existed {
		t.Fatalf("a directory read as a file: existed=%v err=%v", existed, err)
	}
	if _, existed, err := readIfExists(filepath.Join(dir, "missing")); err != nil || existed {
		t.Fatalf("missing file: existed=%v err=%v", existed, err)
	}
}
