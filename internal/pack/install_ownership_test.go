package pack

import (
	"os"
	"path/filepath"
	"testing"
)

// ownWrite creates a file, and its parent folders, with the given body.
func ownWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ownPack returns a one-item pack and plan that installs folder name from src.
func ownPack(wid, name, src string) (*Pack, Plan) {
	it := Item{WorkshopID: wid, Title: "Test " + name, Layer: "base",
		Folders: []Folder{{Name: name, Src: src}}, Bytes: 10, ModIDs: []string{name}}
	return &Pack{Schema: 1, Version: "v2", LoadOrder: []string{wid}, Items: []Item{it}},
		Plan{Profile: "full", Items: []Item{it}}
}

// ownKept reports whether a file called name with exactly body survives anywhere under
// target, either in place or in a backup folder.
func ownKept(t *testing.T, target, name, body string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && info.Name() == name {
			if b, rerr := os.ReadFile(p); rerr == nil && string(b) == body {
				found = true
			}
		}
		return nil
	})
	return found
}

func ownOwns(s state, name string) bool {
	for _, f := range s.Folders {
		if f == name {
			return true
		}
	}
	return false
}

// A run that fails before it takes a folder over must not record that folder as owned.
// Otherwise the next run deletes the user's unrelated folder with the same name
// (PZL-3 finding 1, observation E11).
func TestFailedRunDoesNotOwnUntakenFolder(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(target, "Mine", "user_notes.txt"), "keep me")
	p, pl := ownPack("100", "Mine", "mods/100/Mine")

	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target}); err == nil {
		t.Fatal("run 1 must fail: the source folder is missing")
	}
	if ownOwns(loadState(target), "Mine") {
		t.Fatal("run 1 recorded Mine as owned although it never took the folder over")
	}

	ownWrite(t, filepath.Join(source, "mods", "100", "Mine", "mod.info"), "name=Mine\nid=Mine\n")
	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	if !ownKept(t, target, "user_notes.txt", "keep me") {
		t.Fatal("the user's folder was deleted by the next run")
	}
}

// A non-directory the user keeps at the folder's name is not ours. When a copy fails
// after the user's entry is still in place, the next run must not delete it
// (PZL-3 finding 1, fall-through path: dst exists but is not a directory).
func TestUserFileAtFolderNameSurvivesFailedCopy(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "Mine", "mod.info"), "name=Mine\nid=Mine\n")
	ownWrite(t, filepath.Join(target, "Mine"), "the user's own file")
	p, pl := ownPack("100", "Mine", "mods/100/Mine")

	// Run 1 may fail or succeed depending on the fix. Either way the user's file must survive run 2.
	_, _, _ = Install(p, pl, InstallOptions{Source: source, Target: target})
	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	if !ownKept(t, target, "Mine", "the user's own file") {
		t.Fatal("the user's file at the folder name was deleted")
	}
}

// Folder names come from pack.json. An escaping name must never touch the filesystem
// outside the mods folder (PZL-3 finding 5, observation E7).
func TestInstallRefusesFolderNameOutsideTarget(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(root, "victim", "precious.txt"), "not yours")
	ownWrite(t, filepath.Join(source, "mods", "100", "x", "mod.info"), "name=x\n")
	p, pl := ownPack("100", "../victim", "mods/100/x")

	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target}); err == nil {
		t.Fatal("an escaping folder name must be refused")
	}
	if _, err := os.Stat(filepath.Join(root, "victim", "precious.txt")); err != nil {
		t.Fatal("a folder outside the mods folder was moved or deleted")
	}
}
