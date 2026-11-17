package pack

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Stub folders (the lite profile's client layer) are written as mod.info stubs. Verify must accept
// them, and it must report one that goes missing.
func TestStubLayerInstallsAndVerifies(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "Base", "mod.info"), "name=Base\n")
	base := Item{WorkshopID: "100", Title: "Base", Layer: "base", Bytes: 1, ModIDs: []string{"Base"},
		Folders: []Folder{{Name: "Base", Src: "mods/100/Base"}}}
	client := Item{WorkshopID: "200", Title: "Client only", Layer: "client", Bytes: 1, ModIDs: []string{"Client"},
		Folders: []Folder{{Name: "ClientStub", Src: "mods/200/ClientStub", ModID: "Client"}}}
	p := &Pack{Schema: 1, Version: "v", LoadOrder: []string{"100", "200"},
		Items:    []Item{base, client},
		Profiles: map[string]Profile{"lite": {Layers: []string{"base", "client"}, StubLayer: "client"}}}
	pl, err := BuildPlan(p, "lite")
	if err != nil {
		t.Fatal(err)
	}
	opt := InstallOptions{Source: source, Target: target}
	if _, _, err := Install(p, pl, opt); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "ClientStub", "mod.info")); err != nil {
		t.Fatal("the stub was not written")
	}
	if problems := Verify(pl, opt); len(problems) != 0 {
		t.Fatalf("a stub install reported %v", problems)
	}
	if err := os.Remove(filepath.Join(target, "ClientStub", "mod.info")); err != nil {
		t.Fatal(err)
	}
	if problems := Verify(pl, opt); len(problems) != 1 {
		t.Fatalf("a missing stub must be reported once, got %v", problems)
	}
}

// The free-space query must answer for a writable temporary volume on the platforms that have it.
func TestFreeBytesReadsTheVolume(t *testing.T) {
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "windows":
	default:
		t.Skip("no free-space query on this platform: the space check is skipped")
	}
	free, ok := freeBytes(t.TempDir())
	if !ok || free == 0 {
		t.Fatalf("freeBytes = %d, ok=%v on a writable temporary volume", free, ok)
	}
}
