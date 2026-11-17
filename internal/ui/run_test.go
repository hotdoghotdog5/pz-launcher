package ui

import (
	"runtime"
	"testing"
)

func TestInstallGateWaitsForInFlightInstall(t *testing.T) {
	var g installGate
	if !g.begin() {
		t.Fatal("begin before close must succeed")
	}
	type result struct {
		out installedMsg
		was bool
	}
	done := make(chan result, 1)
	go func() {
		out, was := g.closeAndWait()
		done <- result{out, was}
	}()
	// Wait until the gate is closed, then confirm the close is still waiting on the copy.
	for {
		g.mu.Lock()
		closed := g.closed
		g.mu.Unlock()
		if closed {
			break
		}
		runtime.Gosched()
	}
	select {
	case r := <-done:
		t.Fatalf("closeAndWait returned while a copy was in flight: %+v", r)
	default:
	}
	g.end(installedMsg{count: 7})
	r := <-done
	if !r.was || r.out.count != 7 {
		t.Fatalf("closeAndWait = (count %d, in flight %v), want (7, true)", r.out.count, r.was)
	}
	if g.begin() {
		t.Fatal("begin after close must refuse, so no copy starts unseen")
	}
}

func TestInstallGateIdleCloseReportsNothingInFlight(t *testing.T) {
	var g installGate
	if _, was := g.closeAndWait(); was {
		t.Fatal("no install was running, so nothing should be reported in flight")
	}
}
