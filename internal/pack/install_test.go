package pack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallRefusesEmptyPlan(t *testing.T) {
	_, _, err := Install(&Pack{}, Plan{Profile: "lite"}, InstallOptions{Target: t.TempDir()})
	if err == nil {
		t.Fatal("empty plan must be refused")
	}
}

func TestUninstallKeepsBackups(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "ModA"), 0o755)
	os.MkdirAll(filepath.Join(dir, stateDir, "backup", "x", "UserMod"), 0o755)
	if err := saveState(dir, state{Folders: []string{"ModA"}}); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ModA")); err == nil {
		t.Fatal("owned folder should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, stateDir, "backup", "x", "UserMod")); err != nil {
		t.Fatal("backup must survive uninstall")
	}
}
