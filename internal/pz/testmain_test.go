package pz

import (
	"os"
	"testing"
)

// TestMain keeps the suite off the host's real game state. Without the stub, config
// writes would ask the real process list whether Project Zomboid is running, and on
// the author's Mac it often is. Tests that need a running or failing check install
// their own stub with setGameRunning.
func TestMain(m *testing.M) {
	gameRunningCheck = func() (bool, error) { return false, nil }
	os.Exit(m.Run())
}

// setGameRunning replaces the running-game check for one test.
func setGameRunning(t *testing.T, running bool, err error) {
	t.Helper()
	prev := gameRunningCheck
	gameRunningCheck = func() (bool, error) { return running, err }
	t.Cleanup(func() { gameRunningCheck = prev })
}
