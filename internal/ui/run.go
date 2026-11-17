package ui

import (
	"errors"
	"fmt"
	"os"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

var (
	errAbortedBeforeInstall = errors.New("interrupted before the install started; nothing was written")
	errInstallInterrupted   = errors.New("interrupted during the install")
)

// installGate keeps the process alive until a copy in flight has finished.
//
// pack.Install has no cancellation, so a SIGINT or SIGTERM must wait for the
// copy instead of exiting midway. bubbletea turns those signals into
// InterruptMsg/QuitMsg and returns from Run without calling Update, so the
// keyboard guard in key() cannot see them. A command that starts after Run has
// closed the gate refuses to begin, so nothing is written unseen.
type installGate struct {
	mu       sync.Mutex
	inFlight int
	closed   bool
	outcome  installedMsg
	wg       sync.WaitGroup
}

var gate installGate

// begin registers an install that is about to write. It refuses once closed.
func (g *installGate) begin() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.inFlight++
	g.wg.Add(1)
	return true
}

// end records how the install finished and releases a waiter.
func (g *installGate) end(msg installedMsg) {
	g.mu.Lock()
	g.inFlight--
	g.outcome = msg
	g.mu.Unlock()
	g.wg.Done()
}

// closeAndWait refuses new installs, waits for the one in flight, and reports
// its outcome. The bool is false when no install was running at close.
func (g *installGate) closeAndWait() (installedMsg, bool) {
	g.mu.Lock()
	g.closed = true
	was := g.inFlight > 0
	g.mu.Unlock()
	g.wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.outcome, was
}

// Run shows the wizard until it exits. If a signal ended it during an install,
// Run waits for that copy and reports how it finished.
func Run(m Model) error {
	prog := tea.NewProgram(m, tea.WithAltScreen())
	_, err := prog.Run()
	out, was := gate.closeAndWait()
	if !was {
		return err
	}
	if out.err != nil {
		fmt.Fprintf(os.Stderr, "pzpack: install stopped: %v\n", out.err)
	} else {
		fmt.Fprintf(os.Stderr, "pzpack: install finished after the interrupt: %d mod folders, %.2f GB\n",
			out.count, float64(out.bytes)/1e9)
	}
	fmt.Fprintln(os.Stderr, "pzpack: run pzpack again to verify the target before launching.")
	if err == nil {
		err = errInstallInterrupted
	}
	return err
}
