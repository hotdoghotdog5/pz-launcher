package pack

import (
	"os"
	"path/filepath"
	"testing"
)

// The published pack.json must keep loading. The path validation and the load_order check must
// never reject the real manifest. The test skips when the file is absent.
func TestLoadsPublishedPackManifest(t *testing.T) {
	path := filepath.Join("..", "..", "pack", "pack.json")
	if _, err := os.Stat(path); err != nil {
		t.Skip("published pack.json not present")
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) == 0 {
		t.Fatal("the published manifest has no items")
	}
}
