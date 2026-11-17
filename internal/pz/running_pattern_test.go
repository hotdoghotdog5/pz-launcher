package pz

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TestPgrepFindsGameMainClassOnCommandLine covers the second launch shape: a plain
// java command line that carries the client's main class rather than the game's
// executable name.
func TestPgrepFindsGameMainClassOnCommandLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pgrep is not used on Windows")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not installed")
	}
	// The shell must stay alive with the class name in its argv. A single command
	// would be exec'd and the name dropped, so "cat; :" keeps the shell as a parent.
	// Closing stdin lets cat, then the shell, exit.
	cmd := exec.Command("/bin/sh", "-c", "cat; :", "zombie.gameStates.MainScreenState")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		cmd.Wait()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		pids, err := pgrepPIDs(gameProcessPattern)
		if err != nil {
			t.Fatal(err)
		}
		if containsPID(pids, cmd.Process.Pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pgrep did not report pid %d for the main class", cmd.Process.Pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
