package pack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// packDoc builds a pack.json document whose load_order is loadOrder.
func packDoc(loadOrder []string, items ...string) string {
	q := make([]string, len(loadOrder))
	for i, w := range loadOrder {
		q[i] = fmt.Sprintf("%q", w)
	}
	return fmt.Sprintf(`{"schema":1,"name":"t","version":"v","profiles":{},"load_order":[%s],"items":[%s]}`,
		strings.Join(q, ","), strings.Join(items, ","))
}

// packItem builds one base-layer item with a single folder.
func packItem(wid, name, src string) string {
	return fmt.Sprintf(`{"workshop_id":%q,"title":"t","layer":"base","folders":[{"name":%q,"src":%q}],"bytes":1,"mod_ids":["m"]}`,
		wid, name, src)
}

// Load must reject manifests whose folder names or sources could escape the mods folder,
// collide on case-insensitive filesystems, or disagree with load_order (PZL-3 findings 5, 15, 16).
func TestLoadValidatesManifestPaths(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantErr bool
	}{
		{"valid", packDoc([]string{"100"}, packItem("100", "Mine", "mods/100/Mine")), false},
		{"name escapes mods folder", packDoc([]string{"100"}, packItem("100", "../victim", "mods/100/x")), true},
		{"name is absolute", packDoc([]string{"100"}, packItem("100", "/tmp/victim", "mods/100/x")), true},
		{"name has a separator", packDoc([]string{"100"}, packItem("100", "a/b", "mods/100/x")), true},
		{"src escapes source root", packDoc([]string{"100"}, packItem("100", "Mine", "../../etc")), true},
		{"src is absolute", packDoc([]string{"100"}, packItem("100", "Mine", "/etc")), true},
		{"reserved windows name", packDoc([]string{"100"}, packItem("100", "CON", "mods/100/CON")), true},
		{"reserved name with extension", packDoc([]string{"100"}, packItem("100", "lpt1.txt", "mods/100/x")), true},
		{"trailing dot", packDoc([]string{"100"}, packItem("100", "Mine.", "mods/100/x")), true},
		{"case-only collision", packDoc([]string{"100", "101"},
			packItem("100", "Foo", "mods/100/Foo"), packItem("101", "foo", "mods/101/foo")), true},
		{"load_order names unknown item", packDoc([]string{"100", "999"}, packItem("100", "Mine", "mods/100/Mine")), true},
		{"item missing from load_order", packDoc([]string{"100"},
			packItem("100", "Mine", "mods/100/Mine"), packItem("101", "Other", "mods/101/Other")), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pack.json")
			if err := os.WriteFile(path, []byte(c.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if c.wantErr && err == nil {
				t.Fatal("Load accepted an invalid manifest")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("Load rejected a valid manifest: %v", err)
			}
		})
	}
}
