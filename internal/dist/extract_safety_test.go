package dist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// readOnlyArchive writes a layer whose single entry is read-only (mode 0444).
func readOnlyArchive(t *testing.T, dir string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "read only\n"
	if err := tw.WriteHeader(&tar.Header{Name: "mods/x/ro.txt", Mode: 0o444, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ro.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Re-extracting over a read-only entry from the first run must succeed (PZL-3 finding 21, D5).
func TestExtractReplacesReadOnlyEntry(t *testing.T) {
	dir := t.TempDir()
	archive := readOnlyArchive(t, dir)
	dest := filepath.Join(dir, "out")
	if err := ExtractTarGz(archive, dest); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarGz(archive, dest); err != nil {
		t.Fatalf("second extraction over a read-only entry failed: %v", err)
	}
}

// Extraction must not write through a symlink that already sits in the destination
// (PZL-3 finding 21, observation D6).
func TestExtractRefusesWriteThroughSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "dest")
	if err := os.MkdirAll(filepath.Join(dest, "mods"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, "mods", "x")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	archive := filepath.Join(root, "evil.tar.gz")
	if err := os.WriteFile(archive, makeTarGz(t, map[string]string{"mods/x/evil.txt": "pwned"}), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = ExtractTarGz(archive, dest) // an error is the expected outcome
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("extraction wrote through a pre-existing symlink")
	}
}
