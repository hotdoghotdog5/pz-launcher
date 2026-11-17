package ui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"pzpack/internal/pack"
	"pzpack/internal/pz"
)

func testPack() *pack.Pack {
	return &pack.Pack{
		Name:      "PZ Pack",
		Version:   "2026.11.10",
		GameBuild: "42.21",
		Profiles: map[string]pack.Profile{
			"lite": {Label: "Lite", ApproxBytes: 10 << 30},
			"full": {Label: "Full", ApproxBytes: 11 << 30},
		},
	}
}

func testModel(found bool) Model {
	paths := pz.Paths{
		GameApp:       "/games/pz",
		WorkshopCache: "/games/ws",
		ZomboidDir:    "/zomboid",
		ModsDir:       "/zomboid/mods",
		Found:         found,
	}
	return New(paths, testPack(), "/packs/src", "pack/pack.json", "/zomboid/mods")
}

// press sends one key through Update and returns the new model and command.
func press(m Model, k tea.KeyMsg) (Model, tea.Cmd) {
	next, cmd := m.Update(k)
	return next.(Model), cmd
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func assertContains(t *testing.T, view string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(view, w) {
			t.Errorf("view missing %q", w)
		}
	}
}

func TestWelcomeShowsGameNotFoundAndDisablesLaunch(t *testing.T) {
	assertContains(t, testModel(false).View(),
		"not found", "Launch is disabled until the game is found.", "unknown (game not found)")
}

func TestWelcomeShowsChosenSource(t *testing.T) {
	assertContains(t, testModel(true).View(), "Pack source", "/packs/src")
}

func TestWelcomeWarnsWhenGameRunning(t *testing.T) {
	m := testModel(true)
	m.running = true
	assertContains(t, m.View(), "Project Zomboid is running. Installing is blocked until you quit it.")
}

func TestConflictsReportUnknownWithoutGame(t *testing.T) {
	m := testModel(false)
	m.screen = scrOptions
	m.conflictsKnown = true // even if a scan result exists, without the game it is unknown
	assertContains(t, m.options(), "same-ID check: unknown (game not found)")
}

func TestFailedCardIsHonestAndOffersRetry(t *testing.T) {
	m := testModel(true)
	m.result = installedMsg{err: errors.New("copy 1234: disk full")}
	v := m.failed()
	if strings.Contains(v, "Nothing outside the target folder") {
		t.Fatal("failure card still claims nothing outside the target changed")
	}
	assertContains(t, v, "partly updated", "default-old", "retry")
}

func TestFailedCardSaysNothingWrittenBeforeAWrite(t *testing.T) {
	m := testModel(true)
	m.result = installedMsg{err: errGameRunning, preflight: true}
	v := m.failed()
	assertContains(t, v, "Nothing was written.", errGameRunning.Error())
	if strings.Contains(v, "partly updated") {
		t.Fatal("a pre-write failure must not claim a partial update")
	}
}

func TestDoneCardListsVerifyWarnings(t *testing.T) {
	m := testModel(true)
	m.screen = scrDone
	m.result = installedMsg{count: 3, verify: []string{"default.txt: permission denied", "Zomboid: empty"}}
	assertContains(t, m.done(),
		"Install finished with 2 warning(s).", "default.txt: permission denied", "Zomboid: empty")
}

func TestDoneCardCapsLongWarningLists(t *testing.T) {
	var warnings []string
	for i := 0; i < maxListed+3; i++ {
		warnings = append(warnings, fmt.Sprintf("problem %02d", i))
	}
	m := testModel(true)
	m.result = installedMsg{verify: warnings}
	assertContains(t, m.done(), "… and 3 more")
}

func TestLaunchKeyRefusedWithoutGame(t *testing.T) {
	m := testModel(false)
	m.screen = scrDone
	next, cmd := press(m, runes("l"))
	if cmd != nil {
		t.Fatal("launch must not start without the game")
	}
	if !strings.Contains(next.launchNote, "not found") {
		t.Fatalf("launchNote = %q, want a not-found explanation", next.launchNote)
	}
}

func TestLaunchKeyRefusedAfterAStart(t *testing.T) {
	m := testModel(true)
	m.screen = scrDone
	m.launched = true
	if _, cmd := press(m, runes("l")); cmd != nil {
		t.Fatal("a second launch in the same session must be refused")
	}
}

func TestLaunchCheckboxStartsGameOnCleanInstall(t *testing.T) {
	m := testModel(true)
	m.screen = scrPlan
	m.launch = true
	next, cmd := m.Update(installedMsg{count: 2})
	if cmd == nil {
		t.Fatal("a clean install with the launch box checked must start the launch step")
	}
	if next.(Model).screen != scrDone {
		t.Fatalf("screen = %v, want the done card", next.(Model).screen)
	}
}

func TestLaunchSkippedAfterWarnings(t *testing.T) {
	m := testModel(true)
	m.screen = scrPlan
	m.launch = true
	next, cmd := m.Update(installedMsg{count: 2, verify: []string{"x: empty"}})
	if cmd != nil {
		t.Fatal("warnings must leave the launch to an explicit l press")
	}
	assertContains(t, next.(Model).done(), "Launch skipped because of the warnings above.")
}

func TestLaunchCheckboxIgnoredWithoutGame(t *testing.T) {
	m := testModel(false)
	m.screen = scrOptions
	m.cursor = 1
	if next, _ := press(m, runes(" ")); next.launch {
		t.Fatal("the launch box must not toggle without the game")
	}
}

func TestCtrlCRefusedWhileInstalling(t *testing.T) {
	m := testModel(true)
	m.screen = scrInstalling
	next, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil || next.quitting {
		t.Fatal("ctrl+c must not quit during an install")
	}
}

func TestCtrlCQuitsOutsideInstall(t *testing.T) {
	m := testModel(true)
	m.screen = scrWelcome
	next, cmd := press(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil || !next.quitting {
		t.Fatal("ctrl+c must quit outside an install")
	}
}

func TestFailedRetryReturnsToPlan(t *testing.T) {
	m := testModel(true)
	m.screen = scrFailed
	m.plan = pack.Plan{Profile: "lite"}
	m.result = installedMsg{err: errors.New("copy failed")}
	next, _ := press(m, runes("r"))
	if next.screen != scrPlan || next.result.err != nil {
		t.Fatalf("retry: screen %v, error %v; want plan with no error", next.screen, next.result.err)
	}
}

func TestFailedRetryBeforePlanReturnsToProfile(t *testing.T) {
	m := testModel(true)
	m.screen = scrFailed
	m.result = installedMsg{err: errors.New("bad plan"), preflight: true}
	if next, _ := press(m, runes("r")); next.screen != scrProfile {
		t.Fatalf("retry before any plan: screen %v, want the profile screen", next.screen)
	}
}

func TestConflictScanRunsAsCommand(t *testing.T) {
	m := testModel(true)
	m.screen = scrProfile
	next, cmd := press(m, tea.KeyMsg{Type: tea.KeyEnter})
	if next.screen != scrOptions || cmd == nil {
		t.Fatalf("profile enter: screen %v, cmd nil=%v; want options with a scan command", next.screen, cmd == nil)
	}
	if next.conflictsKnown {
		t.Fatal("the same-ID scan must not run synchronously in Update")
	}
	done, _ := next.Update(conflictsMsg{conflicts: []pack.Conflict{{ID: "A", Source: "workshop", Other: "/x"}}})
	got := done.(Model)
	if !got.conflictsKnown || len(got.conflict) != 1 {
		t.Fatalf("after scan: known=%v, %d conflicts; want known with 1", got.conflictsKnown, len(got.conflict))
	}
}
