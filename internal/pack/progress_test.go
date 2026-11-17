package pack

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// callRecord is one progress callback of an install or a verification.
type callRecord struct {
	done, total int64
	label       string
}

// recordInto returns a progress callback that appends every call to calls.
func recordInto(calls *[]callRecord) func(done, total int64, label string) {
	return func(done, total int64, label string) {
		*calls = append(*calls, callRecord{done, total, label})
	}
}

// labelIndex returns the position of the first call with label, or -1.
func labelIndex(calls []callRecord, label string) int {
	for i, c := range calls {
		if c.label == label {
			return i
		}
	}
	return -1
}

// countLabel returns how many calls carry label.
func countLabel(calls []callRecord, label string) int {
	n := 0
	for _, c := range calls {
		if c.label == label {
			n++
		}
	}
	return n
}

// Verify calls the progress callback once per folder, numbered from one over the folders it checks.
func TestVerifyReportsOneLinePerFolder(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "A", "a.lua"), "a")
	ownWrite(t, filepath.Join(source, "mods", "100", "B", "b.lua"), "bb")
	p, pl := twoFolderPack()
	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	var calls []callRecord
	opt := InstallOptions{Source: source, Target: target, Progress: recordInto(&calls)}
	if problems := Verify(pl, opt); len(problems) != 0 {
		t.Fatalf("clean install reported %v", problems)
	}
	want := []callRecord{{1, 2, "verifying · folder 1 of 2"}, {2, 2, "verifying · folder 2 of 2"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("verify progress = %v, want %v", calls, want)
	}
}

// A stub folder is a folder of the plan, so it counts in the total and gets its own line.
func TestVerifyCountsStubFolders(t *testing.T) {
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
	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	var calls []callRecord
	opt := InstallOptions{Source: source, Target: target, Progress: recordInto(&calls)}
	if problems := Verify(pl, opt); len(problems) != 0 {
		t.Fatalf("stub install reported %v", problems)
	}
	want := []callRecord{{1, 2, "verifying · folder 1 of 2"}, {2, 2, "verifying · folder 2 of 2"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("verify progress = %v, want %v", calls, want)
	}
}

// Install labels every folder with its phase, and announces the cleanup of folders that left the pack once,
// before the state is saved, and the save itself once.
func TestInstallAnnouncesFoldersAndCleanup(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "A", "mod.info"), "name=A\n")
	ownWrite(t, filepath.Join(source, "mods", "100", "B", "mod.info"), "name=B\n")
	p, pl := twoFolderPack()
	var first []callRecord
	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target, Progress: recordInto(&first)}); err != nil {
		t.Fatal(err)
	}
	if len(first) == 0 {
		t.Fatal("the install announced nothing")
	}
	for _, c := range first {
		if c.label == "finishing · saving state" {
			continue
		}
		if !strings.HasPrefix(c.label, "installing · ") {
			t.Fatalf("install label %q has no phase; a bare folder name prints one console line per folder", c.label)
		}
	}
	if n := countLabel(first, "finishing · saving state"); n != 1 {
		t.Fatalf("saving state announced %d times, want once", n)
	}

	// The second pack keeps only folder A. B was ours and left the pack, so it goes, and the console says so.
	p2, pl2 := ownPack("100", "A", "mods/100/A")
	var second []callRecord
	if _, _, err := Install(p2, pl2, InstallOptions{Source: source, Target: target, Progress: recordInto(&second)}); err != nil {
		t.Fatal(err)
	}
	removing := labelIndex(second, "removing folders that left the pack")
	saving := labelIndex(second, "finishing · saving state")
	if removing < 0 || saving < 0 || removing > saving {
		t.Fatalf("cleanup announcement is missing or out of order: %v", second)
	}
	if n := countLabel(second, "removing folders that left the pack"); n != 1 {
		t.Fatalf("cleanup announced %d times, want once", n)
	}
	if c := second[removing]; c.done != 0 || c.total != 0 {
		t.Fatalf("cleanup call reports (%d, %d); a label alone must report (0, 0)", c.done, c.total)
	}
}

// A run that removes nothing announces no cleanup.
func TestInstallWithNothingToRemoveAnnouncesNoCleanup(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "mods")
	ownWrite(t, filepath.Join(source, "mods", "100", "A", "mod.info"), "name=A\n")
	p, pl := ownPack("100", "A", "mods/100/A")
	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	var again []callRecord
	if _, _, err := Install(p, pl, InstallOptions{Source: source, Target: target, Progress: recordInto(&again)}); err != nil {
		t.Fatal(err)
	}
	if labelIndex(again, "removing folders that left the pack") >= 0 {
		t.Fatalf("a run that removes nothing announced a cleanup: %v", again)
	}
}
