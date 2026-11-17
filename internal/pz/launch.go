package pz

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const ModFoldersArg = "mods,workshop,steam"

// Launch starts Project Zomboid with local mods taking precedence over the Steam
// Workshop cache and author Workshop copies. On Windows and Linux it refuses while
// the game is running, and also when that cannot be checked.
func Launch(p Paths, modsDir string) error {
	return launch(runtime.GOOS, p)
}

// launch is Launch for an explicit OS, so the darwin branch can be tested on any host.
func launch(goos string, p Paths) error {
	if err := p.RequireGame(); err != nil {
		return err
	}
	// Windows and Linux refuse a second copy. macOS does not: open -a activates a game
	// that is already running, which is what the launcher wants there.
	if goos != "darwin" {
		if err := refuseIfGameRunning(); err != nil {
			return err
		}
	}
	switch goos {
	case "darwin":
		if _, err := os.Stat(p.GameApp); err != nil {
			return fmt.Errorf("Project Zomboid.app not found at %s", p.GameApp)
		}
		// Without -n, open activates an already-running game instead of starting a second
		// JVM on the same ~/Zomboid (man open: -n opens a new instance even if one is
		// already running). open returns as soon as LaunchServices accepts the request,
		// so waiting for it does not wait for the game. Its exit status reports a
		// refusal, so a non-zero exit is an error.
		return runLauncher("open", "-a", p.GameApp, "--args", "-modfolders", ModFoldersArg)
	case "windows":
		exe := filepath.Join(p.GameApp, "ProjectZomboid64.exe")
		if _, err := os.Stat(exe); err != nil {
			return fmt.Errorf("ProjectZomboid64.exe not found at %s", exe)
		}
		cmd := exec.Command(exe, "-modfolders", ModFoldersArg)
		cmd.Dir = p.GameApp
		return cmd.Start()
	default:
		sh := filepath.Join(p.GameApp, "projectzomboid.sh")
		if _, err := os.Stat(sh); err != nil {
			return fmt.Errorf("projectzomboid.sh not found at %s", sh)
		}
		cmd := exec.Command("bash", sh, "-modfolders", ModFoldersArg)
		cmd.Dir = p.GameApp
		return cmd.Start()
	}
}

// errAlreadyRunning is returned instead of starting a second game on a platform where
// the launcher cannot hand the request to the running one.
var errAlreadyRunning = errors.New("Project Zomboid is already running; close it first")

// refuseIfGameRunning fails closed. An error from the running check counts as running:
// an unknown state must not start a second JVM on the same ~/Zomboid.
func refuseIfGameRunning() error {
	running, err := gameRunningCheck()
	if err != nil {
		return fmt.Errorf("could not check whether Project Zomboid is running: %w. Close it and retry.", err)
	}
	if running {
		return errAlreadyRunning
	}
	return nil
}

// runLauncher runs a short-lived launcher command to completion. A non-zero exit
// becomes an error that carries the command's output.
func runLauncher(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %w: %s", name, err, msg)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// OpenFolder opens the mods directory in the OS file manager.
func OpenFolder(dir string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", dir).Start()
	case "windows":
		return exec.Command("explorer", dir).Start()
	default:
		return exec.Command("xdg-open", dir).Start()
	}
}

// ClearDefaultMods moves Zomboid/mods/default.txt aside so mods enabled in the main
// menu stop loading ahead of the server's list. The contents are first kept as a
// rotating backup, the same one WriteDefaultMods uses, so RestoreDefaultMods can undo
// the clear. The aside copy gets a unique name, so an earlier aside copy is never
// overwritten. It refuses to run while the game is running.
func ClearDefaultMods(zomboidDir string) error {
	src := DefaultModsPath(zomboidDir)
	cur, existed, err := readIfExists(src)
	if err != nil {
		return err
	}
	if !existed {
		return nil
	}
	if err := guardGameWrite(true); err != nil {
		return err
	}
	if _, err := writeBackup(defaultBackupDir(zomboidDir), filepath.Base(src), cur); err != nil {
		return err
	}
	dst, err := uniqueAsidePath(filepath.Dir(src))
	if err != nil {
		return err
	}
	return replaceFile(src, dst)
}

// uniqueAsidePath returns an unused default-old-<stamp>.txt name in dir.
func uniqueAsidePath(dir string) (string, error) {
	for attempt := 0; attempt < maxUniqueAttempts; attempt++ {
		name := filepath.Join(dir, "default-old-"+nextStamp()+".txt")
		if _, err := os.Lstat(name); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", fmt.Errorf("could not pick an unused name in %s", dir)
}
