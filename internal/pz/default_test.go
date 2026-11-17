package pz

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newZomboidDir returns a fresh Zomboid folder with its mods directory.
func newZomboidDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mods"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRenderDefaultMods(t *testing.T) {
	got := RenderDefaultMods([]string{"Alpha", "Beta"})
	want := "VERSION = 1,\n\nmods\n{\n    mod = Alpha,\n    mod = Beta,\n}\n"
	if got != want {
		t.Fatalf("render mismatch:\n%q\n%q", got, want)
	}
}

func TestWriteDefaultModsBacksUpEachChange(t *testing.T) {
	dir := newZomboidDir(t)
	first := "VERSION = 1,\n\nmods\n{\n    mod = Old,\n}\n"
	if err := os.WriteFile(DefaultModsPath(dir), []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := WriteDefaultMods(dir, []string{"A", "B"})
	if err != nil || !changed {
		t.Fatalf("write: changed=%v err=%v", changed, err)
	}
	body, _ := os.ReadFile(DefaultModsPath(dir))
	if !strings.Contains(string(body), "mod = A,") || strings.Contains(string(body), "Old") {
		t.Fatalf("unexpected content: %s", body)
	}
	if got := backupContents(t, defaultBackupDir(dir), "default.txt"); len(got) != 1 || got[0] != first {
		t.Fatalf("the replaced file should be the one backup, got %q", got)
	}

	changed, err = WriteDefaultMods(dir, []string{"A", "B"})
	if err != nil || changed {
		t.Fatalf("second write should be a no-op: changed=%v err=%v", changed, err)
	}
	if got := backupContents(t, defaultBackupDir(dir), "default.txt"); len(got) != 1 {
		t.Fatalf("a no-op write must not add a backup, have %d", len(got))
	}

	changed, err = WriteDefaultMods(dir, []string{"A", "B", "C"})
	if err != nil || !changed {
		t.Fatalf("third write: changed=%v err=%v", changed, err)
	}
	got := backupContents(t, defaultBackupDir(dir), "default.txt")
	if len(got) != 2 || got[1] != RenderDefaultMods([]string{"A", "B"}) {
		t.Fatalf("the newest backup should hold the list before the last change, got %d backups", len(got))
	}
}

func TestDefaultBackupsRotateAndRestoreNewest(t *testing.T) {
	dir := newZomboidDir(t)
	if err := os.WriteFile(DefaultModsPath(dir), []byte(RenderDefaultMods([]string{"init"})), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 12; i++ {
		if _, err := WriteDefaultMods(dir, []string{fmt.Sprintf("v%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got := backupContents(t, defaultBackupDir(dir), "default.txt")
	if len(got) != backupKeep {
		t.Fatalf("kept %d backups, want %d", len(got), backupKeep)
	}
	if got[len(got)-1] != RenderDefaultMods([]string{"v11"}) {
		t.Fatalf("newest backup is not the list before the last change")
	}
	if got[0] != RenderDefaultMods([]string{"v2"}) {
		t.Fatalf("oldest kept backup is not v2")
	}
	if ok, err := RestoreDefaultMods(dir); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	if b, _ := os.ReadFile(DefaultModsPath(dir)); string(b) != RenderDefaultMods([]string{"v11"}) {
		t.Fatalf("restore did not use the newest backup: %q", b)
	}
}

func TestRestoreDefaultMods(t *testing.T) {
	dir := t.TempDir()
	mods := filepath.Join(dir, "mods")
	os.MkdirAll(filepath.Join(mods, ".pzpack"), 0o755)
	os.WriteFile(filepath.Join(mods, ".pzpack", "default.txt.bak"), []byte("original\n"), 0o644)
	ok, err := RestoreDefaultMods(dir)
	if err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	body, _ := os.ReadFile(filepath.Join(mods, "default.txt"))
	if string(body) != "original\n" {
		t.Fatalf("got %q", body)
	}
}

func TestWriteDefaultModsRejectsEmpty(t *testing.T) {
	if _, err := WriteDefaultMods(t.TempDir(), nil); err == nil {
		t.Fatal("expected an error for an empty list")
	}
}

func TestWriteDefaultModsRefusesBlankList(t *testing.T) {
	dir := newZomboidDir(t)
	orig := RenderDefaultMods([]string{"Keep"})
	if err := os.WriteFile(DefaultModsPath(dir), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]string{{""}, {"  ", "\t"}, {`\`}, {"", `\`, " "}} {
		if changed, err := WriteDefaultMods(dir, ids); err == nil || changed {
			t.Fatalf("blank list %q accepted: changed=%v err=%v", ids, changed, err)
		}
	}
	if b, _ := os.ReadFile(DefaultModsPath(dir)); string(b) != orig {
		t.Fatalf("a blank list overwrote default.txt: %q", b)
	}
	if got := backupContents(t, defaultBackupDir(dir), "default.txt"); len(got) != 0 {
		t.Fatalf("a refused write created %d backups", len(got))
	}
}

func TestWriteDefaultModsRefusesUnreadableFile(t *testing.T) {
	dir := newZomboidDir(t)
	// default.txt is a directory: it exists but cannot be read as a file, so it must
	// not be overwritten.
	if err := os.Mkdir(DefaultModsPath(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteDefaultMods(dir, []string{"A"}); err == nil {
		t.Fatal("expected an error for an unreadable default.txt")
	}
	if st, err := os.Stat(DefaultModsPath(dir)); err != nil || !st.IsDir() {
		t.Fatal("the unreadable entry was replaced")
	}
}

func TestWriteDefaultModsRejectsControlCharacters(t *testing.T) {
	dir := newZomboidDir(t)
	if _, err := WriteDefaultMods(dir, []string{"Good", "Bad\x00Id"}); err == nil {
		t.Fatal("a control character in an id was accepted")
	}
	if _, err := os.Stat(DefaultModsPath(dir)); !os.IsNotExist(err) {
		t.Fatal("default.txt written despite an invalid id")
	}
}

func TestWriteDefaultModsKeepsSpacedIDs(t *testing.T) {
	// Internal spaces are kept as given: whether PZ accepts spaced ids is unverified,
	// so the launcher neither strips nor rejects them.
	dir := newZomboidDir(t)
	if _, err := WriteDefaultMods(dir, []string{"TacHold Pistol", "[J&G] Desert Multicam Uniform"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(DefaultModsPath(dir))
	for _, want := range []string{"    mod = TacHold Pistol,\n", "    mod = [J&G] Desert Multicam Uniform,\n"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in:\n%s", want, b)
		}
	}
}

func TestWriteDefaultModsKeepsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir := newZomboidDir(t)
	path := DefaultModsPath(dir)
	if err := os.WriteFile(path, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteDefaultMods(dir, []string{"A"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640", st.Mode().Perm())
	}
}

func TestWriteDefaultModsRefusesWhileGameRuns(t *testing.T) {
	dir := newZomboidDir(t)
	orig := RenderDefaultMods([]string{"Old"})
	if err := os.WriteFile(DefaultModsPath(dir), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	setGameRunning(t, true, nil)
	if _, err := WriteDefaultMods(dir, []string{"New"}); !errors.Is(err, ErrGameRunning) {
		t.Fatalf("change while running: err=%v, want ErrGameRunning", err)
	}
	if b, _ := os.ReadFile(DefaultModsPath(dir)); string(b) != orig {
		t.Fatal("default.txt changed while the game was running")
	}
	// Re-applying the list that is already current still works.
	if changed, err := WriteDefaultMods(dir, []string{"Old"}); err != nil || changed {
		t.Fatalf("current list while running: changed=%v err=%v", changed, err)
	}
	// A failing check refuses the write rather than assuming the game is closed.
	setGameRunning(t, false, errors.New("no process lister"))
	if _, err := WriteDefaultMods(dir, []string{"New"}); err == nil || errors.Is(err, ErrGameRunning) {
		t.Fatalf("failing check: err=%v, want a refusal that is not ErrGameRunning", err)
	}
}
