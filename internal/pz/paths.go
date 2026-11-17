// Package pz knows where Project Zomboid keeps its files and how to read mod.info.
package pz

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Paths struct {
	OS            string
	ZomboidDir    string   // ~/Zomboid
	ModsDir       string   // ~/Zomboid/mods
	WorkshopCache string   // steamapps/workshop/content/108600
	GameApp       string   // Project Zomboid.app or install dir
	Found         bool     // GameApp passed validGame (real launchable files)
	Searched      []string // Steam libraries/roots that were inspected
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func Detect() Paths {
	p := Paths{OS: runtime.GOOS}
	p.ZomboidDir = filepath.Join(home(), "Zomboid")
	p.Searched = steamLibraries(runtime.GOOS)
	if inst, ok := FindInstall(runtime.GOOS, p.Searched); ok {
		p.GameApp, p.Found = inst.GameApp, true
		p.WorkshopCache = filepath.Join(inst.Library, "steamapps", "workshop", "content", appID)
	} else {
		// Nothing valid: keep a plausible default for display only; callers
		// must check Found / use RequireGame before touching GameApp.
		lib := ""
		if len(p.Searched) > 0 {
			lib = p.Searched[0]
		} else {
			lib = filepath.Join(home(), "Steam")
		}
		p.WorkshopCache = filepath.Join(lib, "steamapps", "workshop", "content", appID)
		p.GameApp = filepath.Join(lib, "steamapps", "common", "ProjectZomboid")
		if runtime.GOOS == "darwin" {
			p.GameApp = filepath.Join(p.GameApp, "Project Zomboid.app")
		}
	}
	// Test/custom installs can point the launcher at another Zomboid tree
	// (default.txt lives there, so tests must never touch the real one).
	if v := os.Getenv("PZPACK_ZOMBOID_DIR"); v != "" {
		p.ZomboidDir = v
	}
	if v := os.Getenv("PZPACK_GAME_DIR"); v != "" {
		p.GameApp = v
		p.Found = validGame(runtime.GOOS, v)
		if lib, ok := libraryOf(v); ok {
			p.WorkshopCache = filepath.Join(lib, "steamapps", "workshop", "content", appID)
		}
	}
	p.ModsDir = filepath.Join(p.ZomboidDir, "mods")
	return p
}

// libraryOf returns the Steam library root above a path containing steamapps/common.
func libraryOf(game string) (string, bool) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(game)), "/")
	for i := len(parts) - 2; i > 0; i-- {
		if strings.EqualFold(parts[i], "common") && strings.EqualFold(parts[i-1], "steamapps") && i-2 >= 0 {
			return filepath.FromSlash(strings.Join(parts[:i-1], "/")), true
		}
	}
	return "", false
}

// RequireGame returns a clear error when no valid install was found.
func (p Paths) RequireGame() error {
	if p.Found || validGame(p.OS, p.GameApp) {
		return nil
	}
	return fmt.Errorf("Project Zomboid install not found (no valid game files in any Steam library). Searched: %s. Set PZPACK_GAME_DIR to the game folder to override", strings.Join(p.Searched, "; "))
}

// ReadModInfo returns the id and name from a mod folder. Build 42 keeps mod.info
// in a version subfolder (42, 42.13, 42.21...) or common.
func ReadModInfo(dir string) (id, name, path string) {
	cands := []string{filepath.Join(dir, "mod.info")}
	for _, sub := range []string{"common", "42", "42.21", "42.20", "42.19", "42.18", "42.17", "42.16", "42.15", "42.14", "42.13", "42.12"} {
		cands = append(cands, filepath.Join(dir, sub, "mod.info"))
	}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "42") {
				cands = append(cands, filepath.Join(dir, e.Name(), "mod.info"))
			}
		}
	}
	for _, c := range cands {
		f, err := os.Open(c)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "id=") && id == "" {
				id = strings.TrimSpace(strings.TrimPrefix(line, "id="))
			}
			if strings.HasPrefix(line, "name=") && name == "" {
				name = strings.TrimSpace(strings.TrimPrefix(line, "name="))
			}
		}
		f.Close()
		if id != "" {
			return id, name, c
		}
	}
	return "", "", ""
}

// ScanModIDs maps mod id -> folder for every mod under root (depth limited).
func ScanModIDs(root string) map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if id, _, _ := ReadModInfo(dir); id != "" {
			out[id] = dir
		}
	}
	return out
}

// WorkshopModIDs scans the Steam workshop cache for mod ids.
func WorkshopModIDs(cache string) map[string]string {
	out := map[string]string{}
	items, err := os.ReadDir(cache)
	if err != nil {
		return out
	}
	for _, it := range items {
		if !it.IsDir() {
			continue
		}
		mods := filepath.Join(cache, it.Name(), "mods")
		if _, err := os.Stat(mods); err != nil {
			mods = filepath.Join(cache, it.Name())
		}
		for id, dir := range ScanModIDs(mods) {
			out[id] = dir
		}
	}
	return out
}

// AuthorWorkshopIDs scans ~/Zomboid/Workshop/<Project>/Contents/mods.
func AuthorWorkshopIDs(zomboidDir string) map[string]string {
	out := map[string]string{}
	root := filepath.Join(zomboidDir, "Workshop")
	projects, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, p := range projects {
		mods := filepath.Join(root, p.Name(), "Contents", "mods")
		for id, dir := range ScanModIDs(mods) {
			out[id] = dir
		}
	}
	return out
}
