package ui

import (
	"fmt"

	"pzpack/internal/pz"
)

// gameRunningCheck is the one running-game check the wizard uses: pz.GameRunning. It is
// a variable only so that tests can put a fake in its place.
var gameRunningCheck = pz.GameRunning

// runningUnknown is the refusal for a running check that could not finish. The check
// fails closed: an unknown state blocks the install and the launch.
func runningUnknown(err error) error {
	return fmt.Errorf("could not check whether Project Zomboid is running: %w. Close it and retry.", err)
}
