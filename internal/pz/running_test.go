package pz

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// setListGameProcesses replaces the process lister for one test.
func setListGameProcesses(t *testing.T, pids []int, err error) {
	t.Helper()
	prev := listGameProcesses
	listGameProcesses = func(string) ([]int, error) { return pids, err }
	t.Cleanup(func() { listGameProcesses = prev })
}

func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

func TestParsePgrepPIDs(t *testing.T) {
	got := parsePgrepPIDs("123\n 456 \n\nnotapid\n0\n")
	if len(got) != 2 || got[0] != 123 || got[1] != 456 {
		t.Fatalf("parsePgrepPIDs = %v, want [123 456]", got)
	}
	if len(parsePgrepPIDs("")) != 0 {
		t.Fatal("empty output should give no pids")
	}
}

func TestParseTasklistPIDs(t *testing.T) {
	out := "\"ProjectZomboid64.exe\",\"4312\",\"Console\",\"1\",\"2,345,678 K\"\r\n" +
		"\"explorer.exe\",\"900\",\"Console\",\"1\",\"100 K\"\r\n"
	if got := parseTasklistPIDs(out); len(got) != 1 || got[0] != 4312 {
		t.Fatalf("parseTasklistPIDs = %v, want [4312]", got)
	}
	none := "INFO: No tasks are running which match the specified criteria.\r\n"
	if got := parseTasklistPIDs(none); len(got) != 0 {
		t.Fatalf("no-match output gave %v", got)
	}
}

func TestGameRunningIgnoresOwnProcess(t *testing.T) {
	setListGameProcesses(t, []int{os.Getpid()}, nil)
	running, err := GameRunning()
	if err != nil || running {
		t.Fatalf("the launcher's own pid was counted: running=%v err=%v", running, err)
	}
}

func TestGameRunningSeesAnotherProcess(t *testing.T) {
	setListGameProcesses(t, []int{os.Getpid(), os.Getpid() + 1}, nil)
	running, err := GameRunning()
	if err != nil || !running {
		t.Fatalf("another process not seen: running=%v err=%v", running, err)
	}
}

func TestGameRunningPropagatesListError(t *testing.T) {
	setListGameProcesses(t, nil, errors.New("boom"))
	running, err := GameRunning()
	if err == nil || running {
		t.Fatalf("list error not reported: running=%v err=%v", running, err)
	}
}

func TestGuardGameWriteConsultsCheckOnlyForChanges(t *testing.T) {
	calls := 0
	prev := gameRunningCheck
	gameRunningCheck = func() (bool, error) {
		calls++
		return true, nil
	}
	t.Cleanup(func() { gameRunningCheck = prev })
	if err := guardGameWrite(false); err != nil {
		t.Fatalf("a no-op write was refused: %v", err)
	}
	if calls != 0 {
		t.Fatal("the running check ran for a no-op write")
	}
	if err := guardGameWrite(true); !errors.Is(err, ErrGameRunning) {
		t.Fatalf("a change while running: err=%v, want ErrGameRunning", err)
	}
}

func TestGuardGameWriteFailsClosed(t *testing.T) {
	setGameRunning(t, false, errors.New("pgrep missing"))
	err := guardGameWrite(true)
	if err == nil || errors.Is(err, ErrGameRunning) {
		t.Fatalf("an unknown running state must refuse the write, got %v", err)
	}
}

func TestPgrepNoMatchIsNotAnError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pgrep is not used on Windows")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not installed")
	}
	pids, err := pgrepPIDs("zz-pzpack-no-such-process-7f3a9c")
	if err != nil || len(pids) != 0 {
		t.Fatalf("no match: pids=%v err=%v; pgrep exit 1 must be an empty result", pids, err)
	}
}

// TestPgrepFindsSpawnedGameExecutable runs a real process whose executable is named
// like the game's and checks that the production pattern finds it.
func TestPgrepFindsSpawnedGameExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pgrep is not used on Windows")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not installed")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found")
	}
	exe := filepath.Join(t.TempDir(), "ProjectZomboid64")
	if err := os.Symlink(sleep, exe); err != nil {
		t.Skip("cannot create a symlink here:", err)
	}
	cmd := exec.Command(exe, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
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
			t.Fatalf("pgrep did not report pid %d for %s", cmd.Process.Pid, gameProcessPattern)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
