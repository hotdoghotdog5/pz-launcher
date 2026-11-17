package ui

import (
	"errors"
	"os"
	"strings"
	"testing"

	"pzpack/internal/pz"
)

// TestMain keeps the suite off the host's real game state. Unless a test installs its
// own fake, the wizard's running check answers "not running" and nothing lists the
// processes on the machine running the tests.
func TestMain(m *testing.M) {
	gameRunningCheck = func() (bool, error) { return false, nil }
	os.Exit(m.Run())
}

// fakeRunningCheck replaces the wizard's running check for one test.
func fakeRunningCheck(t *testing.T, running bool, err error) {
	t.Helper()
	prev := gameRunningCheck
	gameRunningCheck = func() (bool, error) { return running, err }
	t.Cleanup(func() { gameRunningCheck = prev })
}

// installFixture returns a wizard model with an empty target and an empty Zomboid
// folder, and a progress channel for runInstall to close. A write would leave a file
// in one of them.
func installFixture(t *testing.T) (Model, string, string) {
	t.Helper()
	root, zomboid := t.TempDir(), t.TempDir()
	m := testModel(true)
	m.Root = root
	m.Paths.ZomboidDir = zomboid
	m.ch = make(chan progressMsg, 1)
	return m, root, zomboid
}

// launchFixture returns game paths that are found, so launchGame reaches the running
// check, and the empty game folder. A launch config write or a start would leave a
// file there.
func launchFixture(t *testing.T) (pz.Paths, string) {
	t.Helper()
	gameDir := t.TempDir()
	return pz.Paths{GameApp: gameDir, WorkshopCache: t.TempDir(), ZomboidDir: t.TempDir(), Found: true}, gameDir
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("%s is not empty after a refused action: %v", dir, entries)
	}
}

// The welcome card asks the one running check when it opens, and shows its answer.
func TestWelcomeAsksTheRunningCheck(t *testing.T) {
	fakeRunningCheck(t, true, nil)
	msg, ok := testModel(true).Init()().(runningMsg)
	if !ok || !msg.running || msg.err != nil {
		t.Fatalf("Init answered %+v, want the fake's running=true", msg)
	}
}

func TestRunningMessageReachesWelcomeCard(t *testing.T) {
	next, _ := testModel(true).Update(runningMsg{running: true})
	assertContains(t, next.(Model).View(), "Project Zomboid is running. Installing is blocked until you quit it.")
}

func TestWelcomeCardWarnsWhenRunningCheckFails(t *testing.T) {
	next, _ := testModel(true).Update(runningMsg{err: errors.New("pgrep: exec: not found")})
	assertContains(t, next.(Model).View(),
		"could not check whether Project Zomboid is running: pgrep: exec: not found. Close it and retry.")
}

func TestInstallRefusedWhileGameRuns(t *testing.T) {
	fakeRunningCheck(t, true, nil)
	m, root, zomboid := installFixture(t)
	msg := runInstall(m)
	if !msg.preflight || msg.err == nil || !strings.Contains(msg.err.Error(), "Project Zomboid is running") {
		t.Fatalf("install while running: %+v", msg)
	}
	assertEmpty(t, root)
	assertEmpty(t, zomboid)
}

func TestInstallFailsClosedWhenRunningCheckErrors(t *testing.T) {
	fakeRunningCheck(t, false, errors.New("tasklist: exit status 1"))
	m, root, zomboid := installFixture(t)
	msg := runInstall(m)
	want := "could not check whether Project Zomboid is running: tasklist: exit status 1. Close it and retry."
	if !msg.preflight || msg.err == nil || msg.err.Error() != want {
		t.Fatalf("install with a failing check: err=%v preflight=%v", msg.err, msg.preflight)
	}
	assertEmpty(t, root)
	assertEmpty(t, zomboid)
}

func TestLaunchRefusedWhileGameRuns(t *testing.T) {
	fakeRunningCheck(t, true, nil)
	paths, gameDir := launchFixture(t)
	msg := launchGame(paths, t.TempDir(), false)
	if msg.started || !strings.Contains(msg.note, "already running; not starting a second copy") {
		t.Fatalf("launch while running: %+v", msg)
	}
	assertEmpty(t, gameDir)
}

func TestLaunchFailsClosedWhenRunningCheckErrors(t *testing.T) {
	fakeRunningCheck(t, false, errors.New("pgrep: broken"))
	paths, gameDir := launchFixture(t)
	msg := launchGame(paths, t.TempDir(), false)
	want := "could not check whether Project Zomboid is running: pgrep: broken. Close it and retry."
	if msg.started || msg.note != want {
		t.Fatalf("launch with a failing check: %+v", msg)
	}
	assertEmpty(t, gameDir)
}
