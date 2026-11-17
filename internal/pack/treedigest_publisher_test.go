package pack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// When the publisher's repository sits beside this one, the digest of a real published item must
// equal the manifest's tree_sha256. The item root is hashed the way pz-server-modpack/tools/
// verify_manifest.py hashes it. The test skips when that repository is absent.
func TestTreeDigestMatchesPublishedItem(t *testing.T) {
	repo := filepath.Join("..", "..", "..", "pz-server-modpack")
	raw, err := os.ReadFile(filepath.Join(repo, "manifest.json"))
	if err != nil {
		t.Skip("publisher repository not present beside this one")
	}
	var m struct {
		Mods []struct {
			WorkshopID string `json:"workshop_id"`
			Path       string `json:"path"`
			TreeSHA256 string `json:"tree_sha256"`
		} `json:"mods"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Mods {
		if e.WorkshopID != "1962761540" {
			continue
		}
		got, err := treeDigest(filepath.Join(repo, e.Path))
		if err != nil {
			t.Fatal(err)
		}
		if got != e.TreeSHA256 {
			t.Fatalf("item %s digest %s, manifest says %s", e.WorkshopID, got, e.TreeSHA256)
		}
		return
	}
	t.Fatal("item 1962761540 is not in the publisher manifest")
}
