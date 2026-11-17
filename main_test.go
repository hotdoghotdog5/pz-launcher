package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pzpack/internal/pack"
)

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// testPack names one folder at mods/1/mods/X, the layout the real pack uses
// under its source root.
func testPack() *pack.Pack {
	return &pack.Pack{Items: []pack.Item{{Folders: []pack.Folder{{Name: "X", Src: "mods/1/mods/X"}}}}}
}

func TestStepPrinterPrintsEachBucketOnce(t *testing.T) {
	sp := newStepPrinter(5)
	var printed []int64
	for _, pct := range []int64{0, 0, 1, 4, 5, 6, 5, 9, 10, 10, 14, 15} {
		if sp.due(pct) {
			printed = append(printed, pct)
		}
	}
	if want := []int64{0, 5, 10, 15}; !reflect.DeepEqual(printed, want) {
		t.Fatalf("printed %v, want %v", printed, want)
	}
}

func TestWantsHeadlessForEveryFlagTheWizardCannotHonour(t *testing.T) {
	if wantsHeadless(false, "", false, false, false, false, false, 0) {
		t.Fatal("no headless flag set: the wizard should open")
	}
	cases := map[string]bool{
		"no-tui":           wantsHeadless(true, "", false, false, false, false, false, 0),
		"profile":          wantsHeadless(false, "lite", false, false, false, false, false, 0),
		"dry-run":          wantsHeadless(false, "", true, false, false, false, false, 0),
		"clear-default":    wantsHeadless(false, "", false, true, false, false, false, 0),
		"no-default":       wantsHeadless(false, "", false, false, true, false, false, 0),
		"launch":           wantsHeadless(false, "", false, false, false, true, false, 0),
		"no-launch-config": wantsHeadless(false, "", false, false, false, false, true, 0),
		"limit":            wantsHeadless(false, "", false, false, false, false, false, 3),
	}
	for name, got := range cases {
		if !got {
			t.Errorf("--%s must route to headless", name)
		}
	}
}

func TestPickLocalSourceRejectsAPlayersModsFolder(t *testing.T) {
	// A player's own Zomboid/mods has a mods/ folder but not the pack's layout.
	zomboid := t.TempDir()
	mustMkdir(t, filepath.Join(zomboid, "mods", "X"))
	packRoot := t.TempDir()
	mustMkdir(t, filepath.Join(packRoot, "mods", "1", "mods", "X"))

	if got := pickLocalSource(testPack(), []string{zomboid}); got != "" {
		t.Fatalf("player's mods folder chosen as source: %q", got)
	}
	if got := pickLocalSource(testPack(), []string{zomboid, packRoot}); got != packRoot {
		t.Fatalf("source = %q, want the pack root %q", got, packRoot)
	}
	if got := pickLocalSource(nil, []string{packRoot}); got != "" {
		t.Fatalf("no manifest loaded but source %q chosen", got)
	}
}

func TestPackResolvesReportsTheFirstMissingFolder(t *testing.T) {
	root := t.TempDir()
	missing, ok := packResolves(testPack(), root)
	want := filepath.Join(root, "mods", "1", "mods", "X")
	if ok || missing != want {
		t.Fatalf("packResolves = (%q, %v), want (%q, false)", missing, ok, want)
	}
}

func TestPackWithNoFoldersDoesNotResolve(t *testing.T) {
	if _, ok := packResolves(&pack.Pack{}, t.TempDir()); ok {
		t.Fatal("a pack that names no folders must not resolve")
	}
}

// Retry notices print on their own line whatever the bucket, and each bucket still
// prints once. An unknown size prints a retry or a new phase as a label without a
// percentage, and nothing else. A phase is announced once, and a retry does not start a
// phase, so the download that follows a retry prints only when its bucket moves.
func TestProgressPrinterWritesRetryLinesOnTheirOwnLine(t *testing.T) {
	var out bytes.Buffer
	progress := progressPrinter(&out, 10)
	progress(0, 100, "downloading")
	progress(4, 100, "downloading")
	progress(4, 100, "retry 1: connection interrupted at 4 bytes; resuming")
	progress(5, 100, "downloading")
	progress(12, 100, "downloading")
	progress(0, 0, "retry 2: no response for 60s; next try in 2s")
	progress(0, 0, "fetching manifest")
	progress(20, 100, "retry 3: connection closed at 20 of 100 bytes")
	progress(21, 100, "downloading")
	want := "    0%  downloading\n" +
		"    4%  retry 1: connection interrupted at 4 bytes; resuming\n" +
		"   12%  downloading\n" +
		"        retry 2: no response for 60s; next try in 2s\n" +
		"        fetching manifest\n" +
		"   20%  retry 3: connection closed at 20 of 100 bytes\n"
	if got := out.String(); got != want {
		t.Fatalf("progress output:\n%s\nwant:\n%s", got, want)
	}
}

// gameBlock refuses while the game runs, and also when the check cannot finish. Only a
// clean "not running" answer lets a write through.
func TestGameBlockFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		check   func() (bool, error)
		refused []string // what the refusal must say; nil means the write is allowed
	}{
		{"not running", func() (bool, error) { return false, nil }, nil},
		{"running", func() (bool, error) { return true, nil },
			[]string{"install refused", "Project Zomboid is running"}},
		{"check fails", func() (bool, error) { return false, errors.New("tasklist: exit status 1") },
			[]string{"install refused: could not check whether Project Zomboid is running: tasklist: exit status 1. Close it and retry."}},
	}
	for _, c := range cases {
		err := gameBlock("install", c.check)
		if c.refused == nil {
			if err != nil {
				t.Errorf("%s: refused: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: not refused", c.name)
			continue
		}
		for _, want := range c.refused {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %q does not say %q", c.name, err, want)
			}
		}
	}
}
