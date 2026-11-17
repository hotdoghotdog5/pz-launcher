package dist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A new version of the pack changes one item. Only that item's object is requested, the links of the
// unchanged items are not replaced, and the install copies only the folder whose content changed.
func TestDeltaOnlyChangedItemIsFetchedAndUnchangedFoldersStay(t *testing.T) {
	tuneRetries(t, time.Minute)
	site := newItemsSite(t)
	cache := t.TempDir()
	target := filepath.Join(t.TempDir(), "mods")

	itemA := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(21, 4096)}}
	itemC := fxItem{wid: "102", folder: "ModC", layer: "base", files: map[string]string{
		"mod.info": "name=C\n", "data.bin": pseudoBody(23, 4096)}}
	b1 := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{
		"mod.info": "name=B\n", "data.bin": pseudoBody(25, 4096), "old.txt": "removed in v2\n"}}
	b2 := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{
		"mod.info": "name=B\n", "data.bin": pseudoBody(27, 4096)}}

	site.publish(t, "v1", itemA, b1, itemC)
	src, pack1 := mustPrepare(t, site, cache, "lite")
	installPack(t, pack1, src, target, "lite")
	linkA := stampTree(t, filepath.Join(src, "mods", "100"))
	linkC := stampTree(t, filepath.Join(src, "mods", "102"))
	folderA := stampTree(t, filepath.Join(target, "ModA"))
	folderC := stampTree(t, filepath.Join(target, "ModC"))

	site.resetRequests()
	keys2 := site.publish(t, "v2", itemA, b2, itemC)
	src2, pack2 := mustPrepare(t, site, cache, "lite")

	want := "/objects/" + keys2["101"] + ".tar.gz"
	if got := site.objectRequests(); len(got) != 1 || got[0] != want {
		t.Fatalf("objects requested %v, want only %s", got, want)
	}
	if !sameStamps(linkA, stampTree(t, filepath.Join(src2, "mods", "100"))) ||
		!sameStamps(linkC, stampTree(t, filepath.Join(src2, "mods", "102"))) {
		t.Fatal("the link of an unchanged item was replaced")
	}
	if got := mustRead(t, filepath.Join(src2, "mods", "101", ".pzpack-item")); strings.TrimSpace(got) != keys2["101"] {
		t.Fatalf("ModB's link names %q, want the new key", strings.TrimSpace(got))
	}

	installPack(t, pack2, src2, target, "lite")
	if !sameStamps(folderA, stampTree(t, filepath.Join(target, "ModA"))) ||
		!sameStamps(folderC, stampTree(t, filepath.Join(target, "ModC"))) {
		t.Fatal("an unchanged folder was copied again")
	}
	if got := mustRead(t, filepath.Join(target, "ModB", "data.bin")); got != b2.files["data.bin"] {
		t.Fatal("ModB did not take the new content")
	}
	if _, err := os.Stat(filepath.Join(target, "ModB", "old.txt")); !os.IsNotExist(err) {
		t.Fatal("a file the new version removed survived in ModB")
	}
	if got := mustRead(t, filepath.Join(src2, "mods", "101", "mods", "ModB", "data.bin")); got != b2.files["data.bin"] {
		t.Fatal("the source of ModB is not the new content")
	}
	if _, err := os.Stat(filepath.Join(src2, "mods", "101", "mods", "ModB", "old.txt")); !os.IsNotExist(err) {
		t.Fatal("the link of ModB still holds the file the new version removed")
	}
}

// When the manifest publishes a folder's digest, that digest decides whether the folder is kept: a digest
// that differs from the recorded one copies the folder again, and the correct digest keeps it.
func TestPublishedFolderDigestDecidesTheSkip(t *testing.T) {
	tuneRetries(t, time.Minute)
	site := newItemsSite(t)
	cache := t.TempDir()
	target := filepath.Join(t.TempDir(), "mods")
	a := fxItem{wid: "100", folder: "ModA", layer: "base", digest: true, files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(29, 4096)}}

	site.publish(t, "v1", a)
	src, pack1 := mustPrepare(t, site, cache, "lite")
	installPack(t, pack1, src, target, "lite")
	before := stampTree(t, filepath.Join(target, "ModA"))

	wrong := a
	wrong.digest = false
	wrong.published = strings.Repeat("e", 64)
	site.publish(t, "v2", wrong)
	src, pack2 := mustPrepare(t, site, cache, "lite")
	installPack(t, pack2, src, target, "lite")
	if sameStamps(before, stampTree(t, filepath.Join(target, "ModA"))) {
		t.Fatal("a folder whose published digest differs from the recorded one was kept")
	}
	if got := mustRead(t, filepath.Join(target, "ModA", "data.bin")); got != a.files["data.bin"] {
		t.Fatal("ModA differs after the copy")
	}

	settled := stampTree(t, filepath.Join(target, "ModA"))
	site.publish(t, "v3", a)
	src, pack3 := mustPrepare(t, site, cache, "lite")
	installPack(t, pack3, src, target, "lite")
	if !sameStamps(settled, stampTree(t, filepath.Join(target, "ModA"))) {
		t.Fatal("a folder whose published digest matches the recorded one was copied again")
	}
}

// The lite profile fetches the base items only. The full profile also fetches the client items, and the
// lite items are not fetched again.
func TestLiteProfileFetchesBaseItemsOnly(t *testing.T) {
	tuneRetries(t, time.Minute)
	site := newItemsSite(t)
	cache := t.TempDir()
	base := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "name=A\n"}}
	client := fxItem{wid: "200", folder: "ModClient", layer: "client", files: map[string]string{"mod.info": "name=C\n"}}
	keys := site.publish(t, "v1", base, client)

	mustPrepare(t, site, cache, "lite")
	if got := site.objectRequests(); len(got) != 1 || got[0] != "/objects/"+keys["100"]+".tar.gz" {
		t.Fatalf("lite requested %v, want only the base item", got)
	}
	site.resetRequests()
	mustPrepare(t, site, cache, "full")
	if got := site.objectRequests(); len(got) != 1 || got[0] != "/objects/"+keys["200"]+".tar.gz" {
		t.Fatalf("full requested %v, want only the client item", got)
	}
}
