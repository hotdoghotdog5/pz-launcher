// Package ui is the Bubble Tea wizard.
package ui

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"pzpack/internal/pack"
	"pzpack/internal/pz"
)

type screen int

const (
	scrWelcome screen = iota
	scrProfile
	scrOptions
	scrPlan
	scrInstalling
	scrDone
	scrFailed
)

// maxListed caps how many warnings or conflicts a card lists before "… and N more".
const maxListed = 6

var errGameRunning = errors.New("Project Zomboid is running. Quit the game, then retry.")

type progressMsg struct {
	done, total int64
	label       string
}

type installedMsg struct {
	err       error
	preflight bool // failed before any write, so nothing on disk changed
	count     int
	bytes     int64
	verify    []string // warnings: failed pre-steps, verification findings, default.txt errors
}

type conflictsMsg struct{ conflicts []pack.Conflict }

type runningMsg struct {
	running bool
	err     error
}

type launchedMsg struct {
	started bool
	note    string
}

type Model struct {
	Paths    pz.Paths
	Pack     *pack.Pack
	Source   string
	PackPath string
	Root     string

	screen         screen
	cursor         int
	profile        string
	clearDef       bool
	launch         bool
	plan           pack.Plan
	conflict       []pack.Conflict
	conflictsKnown bool
	running        bool
	runningErr     error
	progress       float64
	label          string
	ch             chan progressMsg
	result         installedMsg
	quitting       bool
	launched       bool
	launchNote     string
}

func New(paths pz.Paths, p *pack.Pack, source, packPath, root string) Model {
	return Model{Paths: paths, Pack: p, Source: source, PackPath: packPath, Root: root,
		screen: scrWelcome, profile: "lite"}
}

// Init checks once for a running game so the welcome card can warn before any write.
func (m Model) Init() tea.Cmd { return checkRunningCmd() }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case runningMsg:
		m.running, m.runningErr = msg.running, msg.err
		return m, nil
	case conflictsMsg:
		m.conflict, m.conflictsKnown = msg.conflicts, true
		return m, nil
	case progressMsg:
		if msg.total > 0 {
			m.progress = float64(msg.done) / float64(msg.total)
		}
		m.label = msg.label
		if m.ch != nil {
			return m, waitProgress(m.ch)
		}
		return m, nil
	case installedMsg:
		m.ch = nil
		m.result = msg
		if msg.err != nil {
			m.screen = scrFailed
			return m, nil
		}
		m.screen = scrDone
		// Auto-launch only after a clean install. With warnings, "l" stays the choice.
		if m.launch && m.Paths.Found && len(msg.verify) == 0 {
			return m, launchCmd(m)
		}
		return m, nil
	case launchedMsg:
		m.launched, m.launchNote = msg.started, msg.note
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if m.screen == scrInstalling {
		// Ctrl+C and q are refused mid-copy. Signals are handled in Run.
		return m, nil
	}
	if k == "ctrl+c" || k == "q" {
		m.quitting = true
		return m, tea.Quit
	}
	switch m.screen {
	case scrWelcome:
		if k == "enter" {
			m.screen = scrProfile
		}
	case scrProfile:
		switch k {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < 1 {
				m.cursor++
			}
		case "enter":
			m.profile = "lite"
			if m.cursor == 1 {
				m.profile = "full"
			}
			pl, err := pack.BuildPlan(m.Pack, m.profile)
			if err != nil {
				m.result = installedMsg{err: err, preflight: true}
				m.screen = scrFailed
				return m, nil
			}
			m.plan = pl
			m.conflict, m.conflictsKnown = nil, false
			m.screen = scrOptions
			if m.Paths.Found {
				return m, conflictsCmd(m.Pack, pl, m.Root, m.Paths)
			}
		}
	case scrOptions:
		switch k {
		case "up", "k", "down", "j":
			m.cursor = 1 - m.cursor
		case " ":
			if m.cursor == 0 {
				m.clearDef = !m.clearDef
			} else if m.Paths.Found {
				m.launch = !m.launch
			}
		case "enter":
			m.screen = scrPlan
		case "esc":
			m.screen = scrProfile
		}
	case scrPlan:
		switch k {
		case "enter":
			m.screen = scrInstalling
			m.ch = make(chan progressMsg, 128)
			return m, tea.Batch(startInstall(m), waitProgress(m.ch))
		case "esc":
			m.screen = scrOptions
		}
	case scrDone:
		switch k {
		case "l":
			switch {
			case m.launched:
				m.launchNote = "already launched this session; close the game before launching again"
			case !m.Paths.Found:
				m.launchNote = "launch unavailable: Project Zomboid not found (set PZPACK_GAME_DIR)"
			default:
				m.launchNote = ""
				return m, launchCmd(m)
			}
		case "o":
			if err := pz.OpenFolder(m.Paths.ModsDir); err != nil {
				m.launchNote = "open folder: " + err.Error()
			}
		case "enter":
			return m, tea.Quit
		}
	case scrFailed:
		switch k {
		case "r":
			m.result = installedMsg{}
			m.screen = scrPlan
			if m.plan.Profile == "" {
				m.screen = scrProfile
			}
		case "enter":
			return m, tea.Quit
		}
	}
	return m, nil
}

func waitProgress(ch chan progressMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil // channel closed: stop the progress loop
		}
		return msg
	}
}

func checkRunningCmd() tea.Cmd {
	return func() tea.Msg {
		running, err := gameRunningCheck()
		return runningMsg{running: running, err: err}
	}
}

// conflictsCmd runs the same-ID scan off the UI loop.
func conflictsCmd(p *pack.Pack, pl pack.Plan, root string, paths pz.Paths) tea.Cmd {
	return func() tea.Msg {
		return conflictsMsg{conflicts: pack.FindConflicts(p, pl, root, paths.WorkshopCache, paths.ZomboidDir)}
	}
}

// startInstall runs the copy off the UI loop. It registers with the gate first,
// so a signal waits for the copy instead of abandoning it.
func startInstall(m Model) tea.Cmd {
	return func() tea.Msg {
		if !gate.begin() {
			return installedMsg{err: errAbortedBeforeInstall, preflight: true}
		}
		msg := runInstall(m)
		gate.end(msg)
		return msg
	}
}

func runInstall(m Model) installedMsg {
	defer close(m.ch)
	// Checked here as well as on the welcome card, so a game started after the
	// welcome screen still blocks the write. A check that cannot finish blocks too.
	running, err := gameRunningCheck()
	if err != nil {
		return installedMsg{err: runningUnknown(err), preflight: true}
	}
	if running {
		return installedMsg{err: errGameRunning, preflight: true}
	}
	var pre []string
	if m.clearDef {
		if err := pz.ClearDefaultMods(m.Paths.ZomboidDir); err != nil {
			pre = append(pre, "clear default.txt: "+err.Error())
		}
	}
	opt := pack.InstallOptions{
		Source: m.Source, Target: m.Root,
		Progress: func(done, total int64, label string) {
			select {
			case m.ch <- progressMsg{done, total, label}:
			default:
			}
		},
	}
	count, bytes, err := pack.Install(m.Pack, m.plan, opt)
	if err != nil {
		return installedMsg{err: err, count: count, bytes: bytes, verify: pre}
	}
	verify := append(pre, pack.Verify(m.plan, opt)...)
	if !m.clearDef {
		ids, err := m.Pack.ModIDsFor(m.profile)
		if err == nil {
			_, err = pz.WriteDefaultMods(m.Paths.ZomboidDir, ids)
		}
		if err != nil {
			verify = append(verify, "default.txt: "+err.Error())
		}
	}
	return installedMsg{count: count, bytes: bytes, verify: verify}
}

func launchCmd(m Model) tea.Cmd {
	paths, root, full := m.Paths, m.Root, m.profile == "full"
	return func() tea.Msg { return launchGame(paths, root, full) }
}

// launchGame starts the game with local mods first. It refuses a second copy,
// and it writes the launch config only when the game is not running.
func launchGame(paths pz.Paths, root string, full bool) launchedMsg {
	if !paths.Found {
		return launchedMsg{note: "launch unavailable: Project Zomboid not found (set PZPACK_GAME_DIR)"}
	}
	running, err := gameRunningCheck()
	if err != nil {
		return launchedMsg{note: runningUnknown(err).Error()}
	}
	if running {
		return launchedMsg{note: "Project Zomboid is already running; not starting a second copy"}
	}
	if _, err := pz.ApplyLaunchConfig(paths, root, full); err != nil {
		return launchedMsg{note: "launch config: " + err.Error()}
	}
	if err := pz.Launch(paths, root); err != nil {
		return launchedMsg{note: "launch: " + err.Error()}
	}
	return launchedMsg{started: true}
}

var (
	accent  = lipgloss.Color("#F25F8B")
	teal    = lipgloss.Color("#5EEAD4")
	muted   = lipgloss.Color("#9CA3AF")
	fg      = lipgloss.Color("#E5E7EB")
	bg      = lipgloss.Color("#111827")
	cardBg  = lipgloss.Color("#1F2937")
	okGreen = lipgloss.Color("#34D399")
	amber   = lipgloss.Color("#FBBF24")
	red     = lipgloss.Color("#F87171")
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	subStyle   = lipgloss.NewStyle().Foreground(muted)
	keyStyle   = lipgloss.NewStyle().Foreground(teal).Bold(true)
	textStyle  = lipgloss.NewStyle().Foreground(fg)
	cardStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).
			Padding(1, 2).Background(cardBg)
	selStyle  = lipgloss.NewStyle().Foreground(bg).Background(accent).Bold(true).Padding(0, 1)
	dimStyle  = lipgloss.NewStyle().Foreground(muted)
	okStyle   = lipgloss.NewStyle().Foreground(okGreen).Bold(true)
	warnStyle = lipgloss.NewStyle().Foreground(amber)
	errStyle  = lipgloss.NewStyle().Foreground(red)
)

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	var body string
	switch m.screen {
	case scrWelcome:
		body = m.welcome()
	case scrProfile:
		body = m.profiles()
	case scrOptions:
		body = m.options()
	case scrPlan:
		body = m.planView()
	case scrInstalling:
		body = m.installing()
	case scrDone:
		body = m.done()
	case scrFailed:
		body = m.failed()
	}
	footer := dimStyle.Render("  q quit · ↑/↓ move · enter select · space toggle")
	if m.screen == scrInstalling {
		footer = dimStyle.Render("  copy in progress · keys are ignored until it finishes")
	}
	return lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.NewStyle().Padding(0, 1).Render(m.header()),
		body,
		footer,
	)
}

func (m Model) header() string {
	return lipgloss.JoinHorizontal(lipgloss.Center,
		titleStyle.Render("PZ SERVER MODPACK"), "  ",
		subStyle.Render(fmt.Sprintf("v%s · PZ %s", m.Pack.Version, m.Pack.GameBuild)))
}

// runningNote warns when the game holds the mods. A check that could not finish
// blocks the install, so the card says that too.
func (m Model) runningNote() string {
	switch {
	case m.running:
		return warnStyle.Render("! Project Zomboid is running. Installing is blocked until you quit it.")
	case m.runningErr != nil:
		return warnStyle.Render("! " + runningUnknown(m.runningErr).Error())
	}
	return ""
}

// conflictNote reports the same-ID check. Without the game the scan cannot run,
// so the result is unknown rather than an empty list.
func (m Model) conflictNote() string {
	switch {
	case !m.Paths.Found:
		return warnStyle.Render("! same-ID check: unknown (game not found)")
	case !m.conflictsKnown:
		return dimStyle.Render("same-ID check: running…")
	case len(m.conflict) == 0:
		return dimStyle.Render("same-ID check: no mods outside this pack")
	}
	return warnStyle.Render(fmt.Sprintf("! %d same-ID mods found outside this pack", len(m.conflict)))
}

func (m Model) welcome() string {
	game := textStyle.Render(m.Paths.GameApp)
	workshop := textStyle.Render(m.Paths.WorkshopCache)
	if !m.Paths.Found {
		game = errStyle.Render("not found") + dimStyle.Render(" · set PZPACK_GAME_DIR to the game folder")
		workshop = dimStyle.Render("unknown (game not found)")
	}
	source := textStyle.Render(displayPath(m.Source))
	if m.Source == "" {
		source = errStyle.Render("none found") + dimStyle.Render(" · pass --source or set a manifest URL")
	}
	lines := []string{
		textStyle.Render("A frozen, patched modpack for the server."),
		"",
		dimStyle.Render("Project Zomboid") + "   " + game,
		dimStyle.Render("Mods folder") + "      " + textStyle.Render(m.Root),
		dimStyle.Render("Workshop cache") + "   " + workshop,
		dimStyle.Render("Pack source") + "      " + source,
	}
	if note := m.runningNote(); note != "" {
		lines = append(lines, "", note)
	}
	lines = append(lines, "",
		subStyle.Render("Installs local copies and launches the game with local mods first,"),
		subStyle.Render("so Steam Workshop copies can never overwrite or supersede them."))
	if !m.Paths.Found {
		lines = append(lines, "", errStyle.Render("Launch is disabled until the game is found."))
	}
	return cardStyle.Render(strings.Join(lines, "\n")) + "\n\n" +
		keyStyle.Render("  enter") + textStyle.Render("  continue")
}

func (m Model) profiles() string {
	lite := m.Pack.Profiles["lite"]
	full := m.Pack.Profiles["full"]
	rows := []string{
		m.profileRow(0, "Lite", lite.Label, lite.ApproxBytes, "base game + server content"),
		m.profileRow(1, "Full", full.Label, full.ApproxBytes, "adds Viewpoint, ZombieBuddy and 12 visual mods"),
	}
	return cardStyle.Render(strings.Join(rows, "\n\n")) + "\n\n" +
		keyStyle.Render("  enter") + textStyle.Render("  choose profile")
}

func (m Model) profileRow(i int, name, desc string, bytes int64, sub string) string {
	cursor := "  "
	style := textStyle
	if m.cursor == i {
		cursor = "▸ "
		style = selStyle
	}
	head := style.Render(cursor + name)
	return head + "  " + dimStyle.Render(humanBytes(bytes)) + "\n    " +
		textStyle.Render(desc) + "\n    " + dimStyle.Render(sub)
}

func (m Model) options() string {
	check := func(b bool) string {
		if b {
			return okStyle.Render("[x]")
		}
		return dimStyle.Render("[ ]")
	}
	launchRow := m.optRow(1, check(m.launch), "Launch Project Zomboid after install",
		"starts the game with -modfolders mods,workshop,steam")
	if !m.Paths.Found {
		launchRow = m.optRow(1, dimStyle.Render("[ ]"), "Launch Project Zomboid after install",
			"unavailable: game not found (set PZPACK_GAME_DIR)")
	}
	rows := []string{
		textStyle.Render("Install target") + "  " + dimStyle.Render(m.Root),
		"",
		m.optRow(0, check(m.clearDef), "Do not set main-menu load order",
			"skips writing default.txt and moves the existing list aside (backup kept)"),
		launchRow,
		"",
		m.conflictNote(),
	}
	if m.conflictsKnown && len(m.conflict) > 0 {
		rows = append(rows, dimStyle.Render("  local copies still win at launch; see the plan screen"))
	}
	return cardStyle.Render(strings.Join(rows, "\n")) + "\n\n" +
		keyStyle.Render("  enter") + textStyle.Render("  review plan")
}

func (m Model) optRow(i int, box, label, desc string) string {
	cursor := "  "
	if m.cursor == i {
		cursor = "▸ "
	}
	return cursor + box + " " + textStyle.Render(label) + "\n      " + dimStyle.Render(desc)
}

func (m Model) planView() string {
	lines := []string{
		textStyle.Render("Profile") + "   " + okStyle.Render(strings.ToUpper(m.profile)),
		textStyle.Render("Items") + "     " + textStyle.Render(fmt.Sprintf("%d workshop items · %d mod folders", len(m.plan.Items), m.plan.Folders)),
		textStyle.Render("Stubs") + "     " + textStyle.Render(fmt.Sprintf("%d compatibility stubs (no client content)", m.plan.Stubs)),
		textStyle.Render("Download") + "  " + textStyle.Render(humanBytes(m.plan.Bytes)),
		textStyle.Render("Target") + "    " + dimStyle.Render(m.Root),
	}
	if note := m.runningNote(); note != "" {
		lines = append(lines, "", note)
	}
	lines = append(lines, "", m.conflictNote())
	if m.conflictsKnown && len(m.conflict) > 0 {
		shown := m.conflict
		if len(shown) > maxListed {
			shown = shown[:maxListed]
		}
		for _, c := range shown {
			lines = append(lines, dimStyle.Render("  "+c.ID+"  ("+c.Source+")  "+c.Other))
		}
		if more := len(m.conflict) - len(shown); more > 0 {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  … and %d more", more)))
		}
		lines = append(lines, dimStyle.Render("  These stay untouched. The launcher loads local copies first,"))
		lines = append(lines, dimStyle.Render("  so our patched version wins and Steam updates the other copy."))
	}
	return cardStyle.Render(strings.Join(lines, "\n")) + "\n\n" +
		keyStyle.Render("  enter") + textStyle.Render("  install")
}

func (m Model) installing() string {
	w := 46
	filled := int(m.progress * float64(w))
	if filled > w {
		filled = w
	}
	if filled < 0 {
		filled = 0
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", w-filled)
	return cardStyle.Render(
		textStyle.Render("Installing…") + "\n\n" +
			lipgloss.NewStyle().Foreground(teal).Render(bar) + "  " +
			textStyle.Render(fmt.Sprintf("%3.0f%%", m.progress*100)) + "\n" +
			dimStyle.Render(m.label) + "\n\n" +
			dimStyle.Render("Ctrl+C is ignored until the copy finishes; the copy cannot be stopped midway."))
}

func (m Model) done() string {
	title := okStyle.Render("Install complete.")
	if n := len(m.result.verify); n > 0 {
		title = warnStyle.Render(fmt.Sprintf("Install finished with %d warning(s).", n))
	}
	lines := []string{
		title,
		"",
		textStyle.Render(fmt.Sprintf("%d mod folders · %s", m.result.count, humanBytes(m.result.bytes))),
		dimStyle.Render("Target " + m.Root),
	}
	if len(m.result.verify) > 0 {
		lines = append(lines, "", warnStyle.Render("Warnings:"))
		lines = append(lines, listLines(m.result.verify)...)
	}
	lines = append(lines, "")
	switch {
	case m.launched:
		lines = append(lines, okStyle.Render("Game launched with local mods first."))
	case m.launch && len(m.result.verify) > 0:
		lines = append(lines, warnStyle.Render("Launch skipped because of the warnings above. Press l to start anyway."))
	}
	launchHint := keyStyle.Render("  l") + textStyle.Render("  launch Project Zomboid")
	switch {
	case !m.Paths.Found:
		launchHint = dimStyle.Render("  l  launch unavailable: game not found")
	case m.launched:
		launchHint = dimStyle.Render("  l  already launched this session")
	}
	lines = append(lines,
		launchHint,
		keyStyle.Render("  o")+textStyle.Render("  open mods folder"),
		keyStyle.Render("  enter")+textStyle.Render("  exit"))
	if m.launchNote != "" {
		lines = append(lines, "", errStyle.Render(m.launchNote))
	}
	return cardStyle.Render(strings.Join(lines, "\n"))
}

// failed shows what a stopped install may have left behind. Only the
// pre-write checks can say nothing was written.
func (m Model) failed() string {
	msg := "unknown error"
	if m.result.err != nil {
		msg = m.result.err.Error()
	}
	note := "The install stopped partway. The target mods folder may be partly updated: owned mod folders are removed and copied again, and default.txt may have been moved aside as default-old*.txt in the Zomboid mods folder. Retry runs the same install again."
	if m.result.preflight {
		note = "Nothing was written."
	}
	lines := []string{
		errStyle.Render("Install failed"),
		"",
		textStyle.Render(msg),
		"",
		dimStyle.Render(note),
	}
	return cardStyle.Render(strings.Join(lines, "\n")) + "\n\n" +
		keyStyle.Render("  r") + textStyle.Render("  retry") + "   " +
		keyStyle.Render("enter") + textStyle.Render("  exit")
}

// listLines renders up to maxListed warning lines with an overflow count.
func listLines(items []string) []string {
	shown := items
	if len(shown) > maxListed {
		shown = shown[:maxListed]
	}
	out := make([]string, 0, len(shown)+1)
	for _, s := range shown {
		out = append(out, dimStyle.Render("  "+s))
	}
	if more := len(items) - len(shown); more > 0 {
		out = append(out, dimStyle.Render(fmt.Sprintf("  … and %d more", more)))
	}
	return out
}

// displayPath shows an absolute path so "." on the card means something.
func displayPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func humanBytes(b int64) string {
	const g = 1 << 30
	const m = 1 << 20
	switch {
	case b >= g:
		return fmt.Sprintf("%.2f GB", float64(b)/g)
	case b >= m:
		return fmt.Sprintf("%.0f MB", float64(b)/m)
	default:
		return fmt.Sprintf("%d KB", b/1024)
	}
}
