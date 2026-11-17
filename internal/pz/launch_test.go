package pz

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stubOpen puts a fake macOS `open` first on PATH. It records its arguments, one per
// line, in the file it returns, then writes msg to stderr and exits with code. That
// lets launch's arguments and its handling of the real command's result be tested
// on any host.
func stubOpen(t *testing.T, code int, msg string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell for the stub")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "open.args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n"
	if msg != "" {
		script += "echo '" + msg + "' 1>&2\n"
	}
	script += fmt.Sprintf("exit %d\n", code)
	if err := os.WriteFile(filepath.Join(dir, "open"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return argsFile
}

func TestLaunchDarwinReportsOpenFailure(t *testing.T) {
	stubOpen(t, 1, "LaunchServices refused")
	app := filepath.Join(t.TempDir(), "Project Zomboid.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	err := launch("darwin", Paths{OS: "darwin", GameApp: app, Found: true})
	if err == nil {
		t.Fatal("open failed but launch reported success")
	}
	if !strings.Contains(err.Error(), "LaunchServices refused") {
		t.Fatalf("error does not carry open's output: %v", err)
	}
}

func TestLaunchDarwinSucceedsWhenOpenSucceeds(t *testing.T) {
	stubOpen(t, 0, "")
	app := filepath.Join(t.TempDir(), "Project Zomboid.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := launch("darwin", Paths{OS: "darwin", GameApp: app, Found: true}); err != nil {
		t.Fatal(err)
	}
}

// TestLaunchDarwinDoesNotStartSecondInstance pins the open arguments. Without -n, open
// activates a game that is already running. With -n it starts a second JVM on the
// same ~/Zomboid (man open: -n opens a new instance even if one is already running).
func TestLaunchDarwinDoesNotStartSecondInstance(t *testing.T) {
	argsFile := stubOpen(t, 0, "")
	app := filepath.Join(t.TempDir(), "Project Zomboid.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := launch("darwin", Paths{OS: "darwin", GameApp: app, Found: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, a := range args {
		if a == "-n" {
			t.Fatalf("open was given -n, which starts a second instance: %q", args)
		}
	}
	if len(args) < 2 || args[0] != "-a" || args[1] != app {
		t.Fatalf("open args = %q, want -a <app> ...", args)
	}
}

func TestLaunchDarwinMissingAppIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.app")
	if err := launch("darwin", Paths{OS: "darwin", GameApp: missing, Found: true}); err == nil {
		t.Fatal("a missing app bundle must be an error")
	}
}

func TestClearDefaultModsBacksUpAndRestores(t *testing.T) {
	dir := newZomboidDir(t)
	orig := RenderDefaultMods([]string{"Alpha", "Beta"})
	if err := os.WriteFile(DefaultModsPath(dir), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ClearDefaultMods(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(DefaultModsPath(dir)); !os.IsNotExist(err) {
		t.Fatal("default.txt still present after clear")
	}
	if ok, err := RestoreDefaultMods(dir); err != nil || !ok {
		t.Fatalf("restore after clear: ok=%v err=%v", ok, err)
	}
	if b, _ := os.ReadFile(DefaultModsPath(dir)); string(b) != orig {
		t.Fatalf("restore did not undo the clear: %q", b)
	}
}

func TestClearDefaultModsTwiceKeepsEveryAsideCopy(t *testing.T) {
	dir := newZomboidDir(t)
	for _, body := range []string{"first\n", "second\n"} {
		if err := os.WriteFile(DefaultModsPath(dir), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ClearDefaultMods(dir); err != nil {
			t.Fatal(err)
		}
	}
	asides, _ := filepath.Glob(filepath.Join(dir, "mods", "default-old-*.txt"))
	if len(asides) != 2 {
		t.Fatalf("aside copies = %d, want 2 (an earlier one was overwritten)", len(asides))
	}
	if ok, err := RestoreDefaultMods(dir); err != nil || !ok {
		t.Fatalf("restore: ok=%v err=%v", ok, err)
	}
	if b, _ := os.ReadFile(DefaultModsPath(dir)); string(b) != "second\n" {
		t.Fatalf("restore should return the newest clear, got %q", b)
	}
}

func TestClearDefaultModsMissingFileIsNoop(t *testing.T) {
	if err := ClearDefaultMods(newZomboidDir(t)); err != nil {
		t.Fatal(err)
	}
}

func TestClearDefaultModsReturnsReadErrors(t *testing.T) {
	dir := newZomboidDir(t)
	// A directory where default.txt should be cannot be read as a file. That is an
	// error, not "nothing to clear".
	if err := os.Mkdir(DefaultModsPath(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ClearDefaultMods(dir); err == nil {
		t.Fatal("expected an error for an unreadable default.txt")
	}
	if st, err := os.Stat(DefaultModsPath(dir)); err != nil || !st.IsDir() {
		t.Fatal("the unreadable entry was moved")
	}
}

func TestClearDefaultModsRefusesWhileGameRuns(t *testing.T) {
	dir := newZomboidDir(t)
	if err := os.WriteFile(DefaultModsPath(dir), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	setGameRunning(t, true, nil)
	if err := ClearDefaultMods(dir); !errors.Is(err, ErrGameRunning) {
		t.Fatalf("clear while running: err=%v, want ErrGameRunning", err)
	}
	if _, err := os.Stat(DefaultModsPath(dir)); err != nil {
		t.Fatal("default.txt moved while the game was running")
	}
}

// fakeGameFolder returns a game folder whose launch script records that it ran. A test
// can then tell a launch from a refusal by whether the marker file exists.
func fakeGameFolder(t *testing.T) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	marker = filepath.Join(dir, "started")
	script := "#!/bin/sh\n: > '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "projectzomboid.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ProjectZomboid64.exe"), []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, marker
}

// Windows and Linux refuse a second copy while the game runs, and never start it.
func TestLaunchRefusesSecondCopyOnLinuxAndWindows(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		dir, marker := fakeGameFolder(t)
		setGameRunning(t, true, nil)
		err := launch(goos, Paths{OS: goos, GameApp: dir, Found: true})
		if !errors.Is(err, errAlreadyRunning) {
			t.Fatalf("%s: err=%v, want errAlreadyRunning", goos, err)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("%s: the game was started while it was already running", goos)
		}
	}
}

// An unknown running state counts as running: the launch is refused, not attempted.
func TestLaunchFailsClosedWhenRunningCheckErrors(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		dir, marker := fakeGameFolder(t)
		setGameRunning(t, false, errors.New("pgrep: exec: not found"))
		err := launch(goos, Paths{OS: goos, GameApp: dir, Found: true})
		want := "could not check whether Project Zomboid is running: pgrep: exec: not found. Close it and retry."
		if err == nil || err.Error() != want {
			t.Fatalf("%s: err=%v, want %q", goos, err, want)
		}
		if _, statErr := os.Stat(marker); statErr == nil {
			t.Fatalf("%s: the game was started on an unknown running state", goos)
		}
	}
}

// macOS hands the request to open -a, which activates a running game, so the launcher
// does not refuse there, whatever the running check says.
func TestLaunchDarwinDoesNotRefuseWhileGameRuns(t *testing.T) {
	argsFile := stubOpen(t, 0, "")
	app := filepath.Join(t.TempDir(), "Project Zomboid.app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	setGameRunning(t, true, nil)
	if err := launch("darwin", Paths{OS: "darwin", GameApp: app, Found: true}); err != nil {
		t.Fatalf("darwin refused while the game runs: %v", err)
	}
	if _, err := os.Stat(argsFile); err != nil {
		t.Fatal("open was not run, so the running game was never handed the request")
	}
}

// With no game running, the Linux launch script runs.
func TestLaunchStartsGameWhenNotRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the launch script is a POSIX shell")
	}
	dir, marker := fakeGameFolder(t)
	setGameRunning(t, false, nil)
	if err := launch("linux", Paths{OS: "linux", GameApp: dir, Found: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the launch script never ran")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
