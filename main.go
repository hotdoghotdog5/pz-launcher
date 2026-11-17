package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"pzpack/internal/dist"
	"pzpack/internal/pack"
	"pzpack/internal/pz"
	"pzpack/internal/ui"
)

// defaultManifestURL is baked into release builds:
//
//	go build -ldflags "-X main.defaultManifestURL=https://.../pzpack/latest.json"
//
// PZPACK_MANIFEST_URL overrides it at runtime.
var defaultManifestURL = ""

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintln(out, "usage: pzpack [flags]")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "With no flags pzpack opens the install wizard. These flags run headless")
	fmt.Fprintln(out, "instead, because the wizard cannot honour them:")
	fmt.Fprintln(out, "  --no-tui --profile --dry-run --clear-default --no-default --launch --no-launch-config --limit")
	fmt.Fprintln(out, "")
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	var (
		source       = flag.String("source", ".", "pack root (contains the mods/ tree)")
		packPath     = flag.String("pack", "pack/pack.json", "path to pack.json")
		root         = flag.String("root", "", "install target (default: detected Zomboid/mods)")
		profile      = flag.String("profile", "", "lite|full; implies --no-tui (empty opens the wizard)")
		dryRun       = flag.Bool("dry-run", false, "plan only, copy nothing; implies --no-tui")
		noTUI        = flag.Bool("no-tui", false, "headless install")
		limit        = flag.Int("limit", 0, "install at most N items (testing); implies --no-tui")
		clearDef     = flag.Bool("clear-default", false, "disable main-menu mods (backs up default.txt); implies --no-tui")
		launch       = flag.Bool("launch", false, "launch the game after install; implies --no-tui")
		uninstall    = flag.Bool("uninstall", false, "remove launcher-managed mods")
		list         = flag.Bool("list", false, "list profiles and exit")
		showVer      = flag.Bool("version", false, "print version and exit")
		serverINI    = flag.String("server-ini", "", "print a server ini fragment for lite|full")
		fromURL      = flag.String("from", "", "manifest URL for a remote install (S3/CDN)")
		cacheDir     = flag.String("cache", "", "download cache (default: OS cache dir/pzpack)")
		refresh      = flag.Bool("refresh", false, "re-download cached layers")
		noDefault    = flag.Bool("no-default", false, "do not rewrite Zomboid/mods/default.txt; implies --no-tui")
		restoreDef   = flag.Bool("restore-default", false, "restore the default.txt backup and exit")
		writeINI     = flag.String("write-server-ini", "", "rewrite Mods=/WorkshopItems= in this server ini and exit")
		localOnly    = flag.Bool("local", false, "use the local --source tree even if a manifest URL is configured")
		downloadOnly = flag.Bool("download-only", false, "download and extract the layers, do not install")
		noLaunchCfg  = flag.Bool("no-launch-config", false, "do not edit Info.plist/ProjectZomboid64.json before launching; implies --no-tui")
		applyCfg     = flag.Bool("apply-launch-config", false, "write heap/ZombieBuddy settings and exit (no launch)")
		where        = flag.Bool("where", false, "print the detected Project Zomboid install and Steam libraries, then exit")
		launchOnly   = flag.Bool("launch-only", false, "launch the game without installing or touching mods")
	)
	flag.Parse()

	explicitSource := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "source" {
			explicitSource = true
		}
	})
	localPack, localPackErr := pack.Load(*packPath)
	localSource := *source
	if !explicitSource {
		localSource = pickLocalSource(localPack, []string{".", filepath.Join("..", "pz-server-modpack")})
	}

	paths := pz.Detect()
	if *root == "" {
		*root = paths.ModsDir
	}
	if *where {
		fmt.Printf("game        %s (valid=%v)\nworkshop    %s\nzomboid dir %s\nmods dir    %s\n", paths.GameApp, paths.Found, paths.WorkshopCache, paths.ZomboidDir, paths.ModsDir)
		fmt.Println("searched:")
		for _, s := range paths.Searched {
			fmt.Println("  ", s)
		}
		if err := paths.RequireGame(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if !*localOnly && *fromURL == "" {
		if v := os.Getenv("PZPACK_MANIFEST_URL"); v != "" {
			*fromURL = v
		} else if defaultManifestURL != "" {
			// Use the frozen tree when it is actually here, otherwise pull the
			// published pack. A local source counts only if the pack's own folders
			// resolve under it.
			if _, err := os.Stat(*packPath); err != nil || localSource == "" {
				*fromURL = defaultManifestURL
			}
		}
	}

	if *restoreDef {
		ok, err := pz.RestoreDefaultMods(paths.ZomboidDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore default:", err)
			os.Exit(1)
		}
		if !ok {
			fmt.Println("no default.txt backup to restore")
		} else {
			fmt.Println("restored Zomboid/mods/default.txt from backup")
		}
		return
	}

	sourceDir := localSource
	packFile := *packPath
	var remote *dist.Manifest
	// Config/launch-only modes need no mod layers: never start a multi-GB download.
	if *fromURL != "" && !*launchOnly && !*applyCfg {
		prof := *profile
		if prof == "" {
			if *noTUI {
				prof = "lite"
			} else {
				prof = "full" // wizard can pick either profile; fetch the client layer too
			}
		}
		progress := progressPrinter(os.Stdout, 5)
		ctx := context.Background()
		if *dryRun || *list || *serverINI != "" || *uninstall || *showVer || *writeINI != "" {
			pp, m, err := dist.FetchPack(ctx, *fromURL, *cacheDir, *refresh, progress)
			if err != nil {
				fmt.Fprintln(os.Stderr, "download:", err)
				os.Exit(1)
			}
			packFile, remote = pp, m
		} else {
			// The space check runs before the first download. The manifest comes first; the cached
			// pack.json is checked by hash and fetched only when missing.
			cache := *cacheDir
			if cache == "" {
				cache = dist.DefaultCacheDir()
			}
			_, est, err := dist.FetchPack(ctx, *fromURL, *cacheDir, false, progress)
			if err != nil {
				fmt.Fprintln(os.Stderr, "download:", err)
				os.Exit(1)
			}
			if err := preflightSpace(dist.EstimateNeed(est, cache, prof, *refresh), cache, *root, pack.FreeBytes, sameVolume); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			src, pp, m, err := dist.Prepare(ctx, *fromURL, *cacheDir, prof, *refresh, progress)
			if err != nil {
				fmt.Fprintln(os.Stderr, "download:", err)
				os.Exit(1)
			}
			sourceDir, packFile, remote = src, pp, m
		}
	}

	if *downloadOnly {
		if *fromURL == "" {
			fmt.Fprintln(os.Stderr, "--download-only needs --from (or a baked manifest URL)")
			os.Exit(1)
		}
		if remote != nil {
			fmt.Printf("downloaded pack %s (PZ %s) to %s\n", remote.PackVersion, remote.GameBuild, sourceDir)
		}
		return
	}

	needsSource := !*list && *serverINI == "" && !*uninstall && !*showVer && !*applyCfg && !*launchOnly && !*restoreDef
	if needsSource && sourceDir == "" && *fromURL == "" {
		fmt.Fprintln(os.Stderr, "no local mod tree found and no manifest URL configured")
		fmt.Fprintln(os.Stderr, "a folder counts only if it holds the pack's own mod folders under mods/")
		if localPackErr != nil {
			fmt.Fprintln(os.Stderr, "pack manifest:", localPackErr)
		}
		fmt.Fprintln(os.Stderr, "run from the pack folder, pass --source ../pz-server-modpack, or pass --from <manifest-url>")
		os.Exit(1)
	}

	defaultProfile := func() string {
		if *profile != "" {
			return *profile
		}
		if ip := pack.InstalledProfile(*root); ip != "" {
			return ip
		}
		return "full"
	}
	if *launchOnly {
		refuseIfRunning("launch")
		prof := defaultProfile()
		if !*noLaunchCfg {
			res, err := pz.ApplyLaunchConfig(paths, *root, prof == "full")
			if err != nil {
				fmt.Fprintln(os.Stderr, "launch config:", err)
				os.Exit(1)
			}
			if len(res.Changed) > 0 {
				fmt.Println("launch config updated:", strings.Join(res.Changed, ", "))
			}
		}
		if err := pz.Launch(paths, *root); err != nil {
			fmt.Fprintln(os.Stderr, "launch:", err)
			os.Exit(1)
		}
		fmt.Println("launched with -modfolders", pz.ModFoldersArg)
		return
	}

	if *applyCfg {
		refuseIfRunning("launch config write")
		prof := defaultProfile()
		res, err := pz.ApplyLaunchConfig(paths, *root, prof == "full")
		if err != nil {
			fmt.Fprintln(os.Stderr, "launch config:", err)
			os.Exit(1)
		}
		agent := "heap only"
		if res.Agent != "" {
			agent = "ZombieBuddy + heap"
		}
		fmt.Printf("launch config: %s, -Xmx%dm\n", agent, res.HeapMB)
		for _, c := range res.Changed {
			fmt.Println("  updated", c)
		}
		if len(res.Changed) == 0 {
			fmt.Println("  already current")
		}
		return
	}

	p, err := pack.Load(packFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if remote != nil && p.Version != remote.PackVersion {
		fmt.Fprintf(os.Stderr, "pack/manifest version mismatch: pack=%s manifest=%s\n", p.Version, remote.PackVersion)
		os.Exit(1)
	}
	if *showVer {
		fmt.Printf("%s %s (PZ %s)\n", p.Name, p.Version, p.GameBuild)
		return
	}
	if *list {
		names := make([]string, 0, len(p.Profiles))
		for name := range p.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			pr := p.Profiles[name]
			fmt.Printf("%-5s %5.2f GB  %s\n", name, float64(pr.ApproxBytes)/1e9, pr.Label)
		}
		return
	}
	if *serverINI != "" {
		ids, err := p.ModIDsFor(*serverINI)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		fmt.Printf("; %s %s — profile %s\n", p.Name, p.Version, *serverINI)
		fmt.Println("; Local mods only: leave WorkshopItems empty so Steam never auto-updates this pack.")
		fmt.Println("WorkshopItems=")
		fmt.Println("Mods=" + strings.Join(ids, ";"))
		return
	}
	if *writeINI != "" {
		prof := *profile
		if prof == "" {
			prof = "full" // a server must list the client layer too
		}
		ids, err := p.ModIDsFor(prof)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		changed, err := pz.WriteServerMods(*writeINI, ids)
		if err != nil {
			fmt.Fprintln(os.Stderr, "server ini:", err)
			os.Exit(1)
		}
		fmt.Printf("server ini: %d mods, WorkshopItems empty (%s)\n", len(ids), map[bool]string{true: "updated", false: "already current"}[changed])
		return
	}
	if *uninstall {
		if err := pack.Uninstall(*root); err != nil {
			fmt.Fprintln(os.Stderr, "uninstall:", err)
			os.Exit(1)
		}
		fmt.Println("removed launcher-managed mods from", *root)
		return
	}

	// A local source must hold the pack's own folders, or the install would fail
	// after writing some of them. Remote sources come from the verified pack.
	if remote == nil && sourceDir != "" {
		if missing, ok := packResolves(p, sourceDir); !ok {
			if missing == "" {
				missing = "(the pack lists no folders)"
			}
			fmt.Fprintf(os.Stderr, "source %s does not hold the pack's mod folders; first missing: %s\n", sourceDir, missing)
			os.Exit(1)
		}
	}

	if wantsHeadless(*noTUI, *profile, *dryRun, *clearDef, *noDefault, *launch, *noLaunchCfg, *limit) {
		runHeadless(p, paths, sourceDir, *root, *profile, *dryRun, *limit, *clearDef, *noDefault, *launch, *noLaunchCfg)
		return
	}

	if err := ui.Run(ui.New(paths, p, sourceDir, packFile, *root)); err != nil {
		fmt.Fprintln(os.Stderr, "tui:", err)
		os.Exit(1)
	}
}

// wantsHeadless reports whether a flag can only be honoured by the headless path.
// The wizard cannot honour any of these, so they must not be silently ignored.
func wantsHeadless(noTUI bool, profile string, dryRun, clearDef, noDefault, launch, noLaunchCfg bool, limit int) bool {
	return noTUI || profile != "" || dryRun || clearDef || noDefault || launch || noLaunchCfg || limit > 0
}

// refuseIfRunning stops a write or a launch while the game holds the mods. It fails
// closed: a check that cannot finish refuses too.
func refuseIfRunning(action string) {
	if err := gameBlock(action, pz.GameRunning); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// gameBlock returns the refusal for a write or a launch while the game may be running.
// It is nil only when the check answers "not running".
func gameBlock(action string, check func() (bool, error)) error {
	running, err := check()
	if err != nil {
		return fmt.Errorf("%s refused: could not check whether Project Zomboid is running: %w. Close it and retry.", action, err)
	}
	if running {
		return fmt.Errorf("%s refused: Project Zomboid is running. Quit the game, then run this again.", action)
	}
	return nil
}

func runHeadless(p *pack.Pack, paths pz.Paths, source, root, profile string, dryRun bool, limit int, clearDef, noDefault, launch, noLaunchCfg bool) {
	if profile == "" {
		profile = "lite"
	}
	plan, err := pack.BuildPlan(p, profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("%s %s · PZ %s\n", p.Name, p.Version, p.GameBuild)
	fmt.Printf("profile   %s\n", profile)
	fmt.Printf("items     %d workshop items / %d mod folders", len(plan.Items), plan.Folders)
	if plan.Stubs > 0 {
		fmt.Printf(" (%d compatibility stubs)", plan.Stubs)
	}
	fmt.Println()
	fmt.Printf("size      %.2f GB\n", float64(plan.Bytes)/1e9)
	fmt.Printf("source    %s\n", source)
	fmt.Printf("target    %s\n", root)

	if !paths.Found {
		fmt.Println("same-id   unknown (Project Zomboid not found; set PZPACK_GAME_DIR)")
	} else if conflicts := pack.FindConflicts(p, plan, root, paths.WorkshopCache, paths.ZomboidDir); len(conflicts) > 0 {
		fmt.Printf("same-id   %d outside the pack (local still wins with -modfolders)\n", len(conflicts))
		for i, c := range conflicts {
			if i >= 5 {
				fmt.Printf("          … %d more\n", len(conflicts)-5)
				break
			}
			fmt.Printf("          %s  %s\n", c.ID, c.Source)
		}
	}
	if dryRun {
		fmt.Println("dry run: nothing copied")
		return
	}
	refuseIfRunning("install")

	failed := false
	clearOK := false
	if clearDef {
		if err := pz.ClearDefaultMods(paths.ZomboidDir); err != nil {
			fmt.Fprintln(os.Stderr, "clear default:", err)
			failed = true
		} else {
			clearOK = true
			fmt.Println("main-menu mod list disabled (default.txt backed up)")
		}
	}
	opt := pack.InstallOptions{
		Source: source, Target: root, Limit: limit,
		Progress: progressPrinter(os.Stdout, 10),
	}
	count, bytes, err := pack.Install(p, plan, opt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "\ninstall:", err)
		fmt.Fprintln(os.Stderr, "the target may be partly updated; run the same command again to finish")
		os.Exit(1)
	}
	fmt.Printf("  done: %d folders, %.2f GB copied", count, float64(bytes)/1e9)
	if plan.Stubs > 0 {
		fmt.Printf(" + %d stubs", plan.Stubs)
	}
	fmt.Println()

	// Install applies --limit to its own copy of the plan, so verify the same slice.
	verifyPlan := plan
	if limit > 0 && len(verifyPlan.Items) > limit {
		verifyPlan.Items = verifyPlan.Items[:limit]
	}
	if problems := pack.Verify(verifyPlan, opt); len(problems) > 0 {
		failed = true
		for _, w := range problems {
			fmt.Fprintln(os.Stderr, "verify:", w)
		}
		fmt.Fprintf(os.Stderr, "verify: %d problem(s) in the target\n", len(problems))
	} else {
		fmt.Println("verify: all folders present")
	}

	switch {
	case clearDef:
		if clearOK {
			fmt.Println("main-menu mod list left empty (default.txt moved aside)")
		}
	case noDefault:
		// The existing main-menu list is kept on purpose.
	case limit > 0:
		fmt.Println("main-menu load order not written: --limit installed a partial set")
	default:
		ids, err := p.ModIDsFor(profile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "default.txt:", err)
			failed = true
			break
		}
		changed, err := pz.WriteDefaultMods(paths.ZomboidDir, ids)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "default.txt:", err)
			failed = true
		case changed:
			fmt.Printf("main-menu load order: %d mods (default.txt updated, previous backed up)\n", len(ids))
		default:
			fmt.Printf("main-menu load order: %d mods (already current)\n", len(ids))
		}
	}

	if failed {
		fmt.Fprintln(os.Stderr, "finished with errors above")
		if launch {
			fmt.Fprintln(os.Stderr, "not launching")
		}
		os.Exit(1)
	}
	if !launch {
		return
	}
	if !noLaunchCfg {
		res, err := pz.ApplyLaunchConfig(paths, root, profile == "full")
		if err != nil {
			fmt.Fprintln(os.Stderr, "launch config:", err)
			if profile == "full" {
				fmt.Fprintln(os.Stderr, "Full needs ZombieBuddy loaded for Viewpoint")
			}
			fmt.Fprintln(os.Stderr, "not launching")
			os.Exit(1)
		}
		agent := "heap only"
		if res.Agent != "" {
			agent = "ZombieBuddy + heap"
		}
		fmt.Printf("launch config: %s, -Xmx%dm", agent, res.HeapMB)
		if len(res.Changed) > 0 {
			fmt.Printf(" (updated %s)", strings.Join(res.Changed, ", "))
		}
		fmt.Println()
	}
	if err := pz.Launch(paths, root); err != nil {
		fmt.Fprintln(os.Stderr, "launch:", err)
		os.Exit(1)
	}
	fmt.Println("launched with -modfolders", pz.ModFoldersArg)
}

// stepPrinter reports whether a progress percentage should print. It prints once
// per step bucket, so a callback that repeats the same percentage prints nothing.
type stepPrinter struct {
	step int64 // percentage points per printed line
	last int64 // bucket of the last printed line; -1 before the first
}

func newStepPrinter(step int64) *stepPrinter {
	return &stepPrinter{step: step, last: -1}
}

// due reports whether pct is in a bucket that has not printed yet, and moves to it.
func (s *stepPrinter) due(pct int64) bool {
	bucket := pct / s.step
	if bucket == s.last {
		return false
	}
	s.last = bucket
	return true
}

// now is the clock the progress printer reads. Tests replace it to drive the heartbeat.
var now = time.Now

// heartbeatEvery is the longest a progress printer stays silent while callbacks keep arriving. It was 10 s;
// on a 1.5 MB/s link the full pack takes about two hours, and 10 s printed about 700 lines for it. Half a
// minute is still well short of the time after which a console looks hung.
const heartbeatEvery = 30 * time.Second

// progressPrinter returns a progress callback that writes to w. A line prints when the percentage
// enters a new step bucket, when the run enters a phase (see phaseOf) it has not printed before, when
// heartbeatEvery has passed since the last line, and always for a retry notice. The bucket is shared by
// every callback, so a run of objects that each end at 100% prints once, not once per object. A phase
// is announced once: download and extraction alternate per item, and a line per switch would print
// hundreds of lines for one pack, so a phase that comes back prints nothing new. A callback with an
// unknown size (total 0) prints its label alone, without a percentage.
func progressPrinter(w io.Writer, step int64) func(done, total int64, label string) {
	sp := newStepPrinter(step)
	entered := map[string]bool{}
	var lastLine time.Time
	return func(done, total int64, label string) {
		var pct int64
		bucket := false
		if total > 0 {
			pct = done * 100 / total
			// due runs on every callback with a size, so a retry line that prints does not also
			// print its bucket a second time.
			bucket = sp.due(pct)
		}
		retry := isRetryLabel(label)
		newPhase := false
		if !retry {
			p := phaseOf(label)
			newPhase = !entered[p]
			entered[p] = true
		}
		t := now()
		if !(retry || bucket || newPhase || t.Sub(lastLine) >= heartbeatEvery) {
			return
		}
		lastLine = t
		if total > 0 {
			fmt.Fprintf(w, "  %3d%%  %s\n", pct, label)
		} else {
			fmt.Fprintf(w, "        %s\n", label)
		}
	}
}

// phaseOf names the phase a progress label is in: the text before the first " · " separator or the first
// digit, whichever comes first, trimmed. "downloading · item 3 of 9" is in "downloading", and
// "linking mods · 5 of 9" is in "linking mods".
func phaseOf(label string) string {
	end := len(label)
	if i := strings.Index(label, " · "); i >= 0 && i < end {
		end = i
	}
	if i := strings.IndexFunc(label, func(r rune) bool { return r >= '0' && r <= '9' }); i >= 0 && i < end {
		end = i
	}
	return strings.TrimSpace(label[:end])
}

// isRetryLabel reports whether a progress label is a download retry notice.
func isRetryLabel(label string) bool {
	return strings.HasPrefix(label, "retry")
}

// pickLocalSource returns the first candidate root that holds the pack's mod
// folders. A bare mods/ folder is not enough: a player's own Zomboid/mods has one.
func pickLocalSource(p *pack.Pack, candidates []string) string {
	if p == nil {
		return ""
	}
	for _, c := range candidates {
		if _, ok := packResolves(p, c); ok {
			return c
		}
	}
	return ""
}

// packResolves reports whether every folder the pack names exists under root,
// and returns the first missing path. A pack with no folders does not resolve.
func packResolves(p *pack.Pack, root string) (string, bool) {
	n := 0
	for _, it := range p.Items {
		if !pack.ItemOnPlatform(it, runtime.GOOS, runtime.GOARCH) {
			continue
		}
		for _, f := range it.Folders {
			n++
			if src := pack.SourcePath(root, f); !isDir(src) {
				return src, false
			}
		}
	}
	return "", n > 0
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
