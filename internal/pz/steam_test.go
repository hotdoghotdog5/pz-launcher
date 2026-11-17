package pz

import (
	"os"
	"path/filepath"
	"testing"
)

const vdf = "\"libraryfolders\"\n{\n\t\"0\"\n\t{\n\t\t\"path\"\t\t\"C:\\\\Program Files (x86)\\\\Steam\"\n\t\t\"apps\"\n\t\t{\n\t\t\t\"228980\"\t\t\"1\"\n\t\t}\n\t}\r\n\t\"1\"\r\n\t{\r\n\t\t\"path\"\t\t\"D:\\\\SteamLibrary\"\r\n\t}\n}\n"

func TestParseLibraryFolders(t *testing.T) {
	got := ParseLibraryFolders(vdf)
	if len(got) != 2 || got[0] != `C:\Program Files (x86)\Steam` || got[1] != `D:\SteamLibrary` {
		t.Fatalf("got %q", got)
	}
}

func TestParseInstallDir(t *testing.T) {
	if d := ParseInstallDir("\"AppState\"\n{\n\t\"installdir\"\t\t\"ProjectZomboid\"\n}\n"); d != "ProjectZomboid" {
		t.Fatal(d)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The real-world bug: game moved to another library, old library keeps a
// ProjectZomboid folder holding only unrelated files (DLSS dlls etc).
func TestFindInstallSkipsStaleLeftoverFolder(t *testing.T) {
	root := t.TempDir()
	c := filepath.Join(root, "C")
	d := filepath.Join(root, "D")
	touch(t, filepath.Join(c, "steamapps", "common", "ProjectZomboid", "nvngx_dlss.dll"))
	touch(t, filepath.Join(d, "steamapps", "common", "ProjectZomboid", "ProjectZomboid64.json"))
	touch(t, filepath.Join(d, "steamapps", "common", "ProjectZomboid", "ProjectZomboid64.exe"))
	touch(t, filepath.Join(d, "steamapps", "appmanifest_108600.acf"))
	for _, libs := range [][]string{{c, d}, {d, c}} {
		inst, ok := FindInstall("windows", libs)
		if !ok || inst.Library != d {
			t.Fatalf("libs %v: got %+v ok=%v", libs, inst, ok)
		}
	}
	if _, ok := FindInstall("windows", []string{c}); ok {
		t.Fatal("stale folder must not count as an install")
	}
}

func TestFindInstallUsesManifestInstallDir(t *testing.T) {
	lib := t.TempDir()
	touch(t, filepath.Join(lib, "steamapps", "appmanifest_108600.acf"))
	os.WriteFile(filepath.Join(lib, "steamapps", "appmanifest_108600.acf"), []byte("\"AppState\"\n{\n\"installdir\"\t\"Zomboid Custom\"\n}\n"), 0o644)
	touch(t, filepath.Join(lib, "steamapps", "common", "Zomboid Custom", "ProjectZomboid64.json"))
	inst, ok := FindInstall("windows", []string{lib})
	if !ok || filepath.Base(inst.GameApp) != "Zomboid Custom" {
		t.Fatalf("%+v %v", inst, ok)
	}
}

func TestRequireGameRejectsLeftovers(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "nvngx_dlss.dll"))
	if err := (Paths{OS: "windows", GameApp: dir}).RequireGame(); err == nil {
		t.Fatal("expected error")
	}
	touch(t, filepath.Join(dir, "ProjectZomboid64.json"))
	if err := (Paths{OS: "windows", GameApp: dir}).RequireGame(); err != nil {
		t.Fatal(err)
	}
}

func TestLibraryOf(t *testing.T) {
	lib, ok := libraryOf(filepath.Join("D:", "SteamLibrary", "steamapps", "common", "ProjectZomboid"))
	if !ok || lib != filepath.Join("D:", "SteamLibrary") {
		t.Fatal(lib, ok)
	}
}
