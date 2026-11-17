package pack

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backupRuns lists the run folders under .pzpack/backup.
func backupRuns(t *testing.T, target string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(target, stateDir, "backup"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	return runs
}

// twoFolderPack is one item with folders A and B, sourced from mods/100/A and mods/100/B.
func twoFolderPack() (*Pack, Plan) {
	it := Item{WorkshopID: "100", Title: "Two", Layer: "base", Bytes: 3, ModIDs: []string{"A", "B"},
		Folders: []Folder{{Name: "A", Src: "mods/100/A"}, {Name: "B", Src: "mods/100/B"}}}
	return &Pack{Schema: 1, Version: "v2", LoadOrder: []string{"100"}, Items: []Item{it}},
		Plan{Profile: "full", Items: []Item{it}}
}

// The digest must equal what pz-server-modpack/tools/verify_manifest.py computes for the same
// files. The expected value comes from running that script on this fixture.
func TestTreeDigestMatchesPublisherAlgorithm(t *testing.T) {
	root := t.TempDir()
	ownWrite(t, filepath.Join(root, "a.txt"), "hello\n")
	ownWrite(t, filepath.Join(root, "sub", "b.txt"), "world\n")
	got, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	const want = "b0810cfea6683eac8b337d0df9ffc6b4880e666e1b8a3c89acf2f0f27af5602e"
	if got != want {
		t.Fatalf("digest %s, publisher algorithm gives %s", got, want)
	}
}

// Dot-entries are never installed, so they must change neither the digest nor the counts.
func TestTreeDigestAndStatsSkipDotEntries(t *testing.T) {
	root := t.TempDir()
	ownWrite(t, filepath.Join(root, "a.txt"), "hello\n")
	ownWrite(t, filepath.Join(root, "sub", "b.txt"), "world\n")
	before, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	ownWrite(t, filepath.Join(root, ".pi", "board.db"), "scratch")
	ownWrite(t, filepath.Join(root, "sub", ".note"), "x")
	after, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("dot-entries changed the digest")
	}
	st, err := statTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.files != 2 || st.bytes != 12 {
		t.Fatalf("stats %+v, want 2 files and 12 bytes", st)
	}
}

// Verify must report folders whose file count or bytes differ from their source, and nothing
// for a clean install.
func TestVerifyReportsMismatches(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "A", "a.lua"), "a")
	ownWrite(t, filepath.Join(source, "mods", "100", "B", "b.lua"), "bb")
	p, pl := twoFolderPack()
	opt := InstallOptions{Source: source, Target: target}
	if _, _, err := Install(p, pl, opt); err != nil {
		t.Fatal(err)
	}
	if problems := Verify(pl, opt); len(problems) != 0 {
		t.Fatalf("clean install reported %v", problems)
	}
	if err := os.Remove(filepath.Join(target, "A", "a.lua")); err != nil {
		t.Fatal(err)
	}
	ownWrite(t, filepath.Join(target, "B", "extra.txt"), "x")
	if problems := Verify(pl, opt); len(problems) != 2 {
		t.Fatalf("want 2 mismatches, got %v", problems)
	}
}

// An owned folder the user changed is moved into backup before it is replaced. An untouched
// owned folder is replaced without leaving anything behind.
func TestOwnedFolderChangedByUserIsBackedUp(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "Mine", "mod.info"), "name=Mine\n")
	ownWrite(t, filepath.Join(source, "mods", "100", "Mine", "scripts", "x.txt"), "original")
	p, pl := ownPack("100", "Mine", "mods/100/Mine")
	opt := InstallOptions{Source: source, Target: target}
	if _, _, err := Install(p, pl, opt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Install(p, pl, opt); err != nil {
		t.Fatal(err)
	}
	if runs := backupRuns(t, target); len(runs) != 0 {
		t.Fatalf("an untouched owned folder produced backups: %v", runs)
	}
	ownWrite(t, filepath.Join(target, "Mine", "scripts", "x.txt"), "user edit")
	if _, _, err := Install(p, pl, opt); err != nil {
		t.Fatal(err)
	}
	if !ownKept(t, target, "x.txt", "user edit") {
		t.Fatal("the user's edit was replaced without a backup")
	}
	if runs := backupRuns(t, target); len(runs) != 1 {
		t.Fatalf("want one backup run, got %v", runs)
	}
}

// Every run that moves user content aside gets its own backup folder, and only the newest
// keepBackups survive.
func TestBackupRunsAreUniqueAndPruned(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "Mine", "mod.info"), "name=Mine\n")
	p, pl := ownPack("100", "Mine", "mods/100/Mine")
	opt := InstallOptions{Source: source, Target: target}
	if _, _, err := Install(p, pl, opt); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= keepBackups+2; i++ {
		ownWrite(t, filepath.Join(target, "Mine", fmt.Sprintf("edit%d.txt", i)), "edit")
		if _, _, err := Install(p, pl, opt); err != nil {
			t.Fatal(err)
		}
	}
	runs := backupRuns(t, target)
	if len(runs) != keepBackups {
		t.Fatalf("kept %d backup runs, want %d: %v", len(runs), keepBackups, runs)
	}
	seen := map[string]bool{}
	for _, r := range runs {
		if seen[r] {
			t.Fatalf("duplicate backup run name %s", r)
		}
		seen[r] = true
	}
}

// A volume without room for the install is refused before anything is copied.
func TestInstallRefusesWhenVolumeIsTooSmall(t *testing.T) {
	old := freeBytesFn
	freeBytesFn = func(string) (uint64, bool) { return 1024, true }
	t.Cleanup(func() { freeBytesFn = old })
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "Mine", "mod.info"), "name=Mine\n")
	p, pl := ownPack("100", "Mine", "mods/100/Mine")
	pl.Items[0].Bytes = 1 << 20
	p.Items[0].Bytes = 1 << 20
	_, _, err := Install(p, pl, InstallOptions{Source: source, Target: target})
	if err == nil || !strings.Contains(err.Error(), "free space") {
		t.Fatalf("want a free-space refusal, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, "Mine")); statErr == nil {
		t.Fatal("the install copied before the space check")
	}
}

// Every source is checked before the first folder is taken over, so a missing one moves
// nothing aside.
func TestInstallChecksSourcesBeforeTakingOver(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(target, "A", "user.txt"), "mine")
	ownWrite(t, filepath.Join(source, "mods", "100", "A", "mod.info"), "name=A\n")
	p, pl := twoFolderPack() // folder B's source does not exist
	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target}); err == nil {
		t.Fatal("a missing source must fail the run")
	}
	if _, err := os.Stat(filepath.Join(target, "A", "user.txt")); err != nil {
		t.Fatal("the user's folder was moved aside before the run failed")
	}
	if runs := backupRuns(t, target); len(runs) != 0 {
		t.Fatalf("backup runs created: %v", runs)
	}
}

// A --limit run installs only the first items and keeps ownership of the rest untouched.
func TestLimitRunKeepsUntouchedFolders(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "A", "mod.info"), "name=A\n")
	ownWrite(t, filepath.Join(source, "mods", "101", "B", "mod.info"), "name=B\n")
	itA := Item{WorkshopID: "100", Layer: "base", Bytes: 1, ModIDs: []string{"A"},
		Folders: []Folder{{Name: "A", Src: "mods/100/A"}}}
	itB := Item{WorkshopID: "101", Layer: "base", Bytes: 1, ModIDs: []string{"B"},
		Folders: []Folder{{Name: "B", Src: "mods/101/B"}}}
	p := &Pack{Schema: 1, Version: "v2", LoadOrder: []string{"100", "101"}, Items: []Item{itA, itB}}
	full := Plan{Profile: "full", Items: []Item{itA, itB}}
	if _, _, err := Install(p, full, InstallOptions{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Install(p, full, InstallOptions{Source: source, Target: target, Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "B", "mod.info")); err != nil {
		t.Fatal("a --limit run deleted a folder it did not replace")
	}
	if !ownOwns(loadState(target), "B") {
		t.Fatal("a --limit run dropped ownership of B")
	}
}
