package pz

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// ErrGameRunning is returned instead of changing a Project Zomboid config file
// while the game is running. The game rewrites some of these files, and it holds
// others open.
var ErrGameRunning = errors.New("Project Zomboid is running; close it before the launcher changes its config files")

// gameProcessPattern matches the game's executable by its exact basename in the
// full command line (pgrep -f, extended regex). On macOS the game runs as
// JavaAppLauncher, the bundle's CFBundleExecutable (verified on a live install).
// The native Linux launcher is ProjectZomboid64, which is unverified on a live
// Linux host. Matching the full command line avoids the 15-character limit that
// Linux applies to process names, which would truncate ProjectZomboid64. A Linux
// install may also run the game as a plain java command line, which carries the
// client's main class; that class is matched too, so either launch shape is seen.
const gameProcessPattern = `(^|/)(JavaAppLauncher|ProjectZomboid64)( |$)|zombie\.gameStates\.MainScreenState`

// windowsGameImage is the image name tasklist reports for the Windows client.
const windowsGameImage = "ProjectZomboid64.exe"

var (
	// gameRunningCheck is what config writes consult. Tests replace it.
	gameRunningCheck = GameRunning
	// listGameProcesses returns the PIDs of running game processes. Tests replace it.
	listGameProcesses = gameProcessPIDs
)

// GameRunning reports whether a Project Zomboid process is running. The launcher's
// own process is never counted. If the check itself fails, the error is returned and
// the bool carries no meaning: callers must treat an error as running (fail closed).
func GameRunning() (bool, error) {
	pids, err := listGameProcesses(runtime.GOOS)
	if err != nil {
		return false, err
	}
	self := os.Getpid()
	for _, pid := range pids {
		if pid != self {
			return true, nil
		}
	}
	return false, nil
}

// gameProcessPIDs lists PIDs of game processes on goos: tasklist on Windows,
// pgrep elsewhere.
func gameProcessPIDs(goos string) ([]int, error) {
	if goos == "windows" {
		out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+windowsGameImage, "/NH", "/FO", "CSV").Output()
		if err != nil {
			return nil, fmt.Errorf("tasklist: %w", err)
		}
		return parseTasklistPIDs(string(out)), nil
	}
	return pgrepPIDs(gameProcessPattern)
}

// pgrepPIDs returns the PIDs whose full command line matches pattern. pgrep exits
// 1 when nothing matches; that is an empty result, not an error.
func pgrepPIDs(pattern string) ([]int, error) {
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep: %w", err)
	}
	return parsePgrepPIDs(string(out)), nil
}

// parsePgrepPIDs reads pgrep's output: one PID per line.
func parsePgrepPIDs(out string) []int {
	var pids []int
	for _, f := range strings.Fields(out) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// parseTasklistPIDs reads `tasklist /FI ... /NH /FO CSV` output. Each row is
// "Image Name","PID",...; lines that are not rows (such as the INFO message when
// nothing matches) are skipped.
func parseTasklistPIDs(out string) []int {
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		rec, err := csv.NewReader(strings.NewReader(strings.TrimSpace(line))).Read()
		if err != nil || len(rec) < 2 || !strings.EqualFold(rec[0], windowsGameImage) {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(rec[1])); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// guardGameWrite refuses a write that would change a file the game owns while the
// game is running. Callers pass changes=false when the write would be a no-op, so
// re-applying a config that is already current still works while the game runs.
// If the running check itself fails, the write is refused: an unknown state is not
// treated as "not running".
func guardGameWrite(changes bool) error {
	if !changes {
		return nil
	}
	running, err := gameRunningCheck()
	if err != nil {
		return fmt.Errorf("could not check whether Project Zomboid is running: %w", err)
	}
	if running {
		return ErrGameRunning
	}
	return nil
}
