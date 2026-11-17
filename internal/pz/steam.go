package pz

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const appID = "108600"

// Install describes a validated Project Zomboid install and the Steam library
// that owns it (workshop content lives in the same library).
type Install struct {
	GameApp string // game dir (Windows/Linux) or Project Zomboid.app (macOS)
	Library string // Steam library root containing steamapps/
}

// kvRe matches one quoted "key" "value" pair on a line of a Valve KeyValues file.
var kvRe = regexp.MustCompile(`^\s*"([^"]+)"\s+"((?:[^"\\]|\\.)*)"\s*$`)

func unescapeVDF(s string) string {
	return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(s)
}

// ParseLibraryFolders returns every library "path" listed in libraryfolders.vdf.
// A tolerant line scanner is enough: only "path" values are needed and the
// file's structure has been stable across Steam client versions.
func ParseLibraryFolders(data string) []string {
	var out []string
	for _, line := range strings.Split(data, "\n") {
		m := kvRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil || !strings.EqualFold(m[1], "path") {
			continue
		}
		if v := strings.TrimSpace(unescapeVDF(m[2])); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ParseInstallDir returns "installdir" from an appmanifest_<id>.acf.
func ParseInstallDir(data string) string {
	for _, line := range strings.Split(data, "\n") {
		m := kvRe.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m != nil && strings.EqualFold(m[1], "installdir") {
			return strings.TrimSpace(unescapeVDF(m[2]))
		}
	}
	return ""
}

// validGame reports whether dir is a real, launchable install, not just a
// folder that happens to exist (Steam leaves stale steamapps/common/ProjectZomboid
// folders behind after a library move; mods/tools also drop files there).
func validGame(goos, dir string) bool {
	isFile := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && st.Mode().IsRegular()
	}
	switch goos {
	case "darwin":
		return isFile(filepath.Join(dir, "Contents", "Info.plist"))
	case "windows":
		return isFile(filepath.Join(dir, "ProjectZomboid64.exe")) || isFile(filepath.Join(dir, "ProjectZomboid64.json"))
	default:
		for _, sub := range []string{"", "projectzomboid"} {
			d := filepath.Join(dir, sub)
			if isFile(filepath.Join(d, "ProjectZomboid64.json")) || isFile(filepath.Join(dir, "projectzomboid.sh")) {
				return true
			}
		}
		return false
	}
}

// gameDirsInLibrary lists candidate game dirs in one library, preferring the
// installdir named by appmanifest_108600.acf (which Steam keeps authoritative).
func gameDirsInLibrary(goos, lib string) []string {
	common := filepath.Join(lib, "steamapps", "common")
	var names []string
	if b, err := os.ReadFile(filepath.Join(lib, "steamapps", "appmanifest_"+appID+".acf")); err == nil {
		if d := ParseInstallDir(string(b)); d != "" {
			names = append(names, d)
		}
	}
	names = append(names, "ProjectZomboid", "Project Zomboid")
	var out []string
	seen := map[string]bool{}
	for _, n := range names {
		d := filepath.Join(common, n)
		if goos == "darwin" {
			d = filepath.Join(d, "Project Zomboid.app")
		}
		if !seen[strings.ToLower(d)] {
			seen[strings.ToLower(d)] = true
			out = append(out, d)
		}
	}
	return out
}

func hasManifest(lib string) bool {
	_, err := os.Stat(filepath.Join(lib, "steamapps", "appmanifest_"+appID+".acf"))
	return err == nil
}

// FindInstall walks every candidate Steam library and returns the first one that
// holds a valid install. Libraries with an appmanifest for the game win over
// those without (a moved game's old library has no manifest any more).
func FindInstall(goos string, libs []string) (Install, bool) {
	libs = dedupePaths(libs)
	sort.SliceStable(libs, func(i, j int) bool { return hasManifest(libs[i]) && !hasManifest(libs[j]) })
	for _, lib := range libs {
		for _, d := range gameDirsInLibrary(goos, lib) {
			if validGame(goos, d) {
				return Install{GameApp: d, Library: lib}, true
			}
		}
	}
	return Install{}, false
}

func dedupePaths(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range in {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		k := p
		if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
			k = strings.ToLower(p)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// steamRoots returns Steam client install roots for the platform, then every
// library listed in each root's libraryfolders.vdf, plus well-known fallbacks.
func steamLibraries(goos string) []string {
	var roots []string
	switch goos {
	case "windows":
		roots = append(roots, windowsRegistrySteamRoots()...)
		roots = append(roots,
			`C:\Program Files (x86)\Steam`, `C:\Program Files\Steam`,
			filepath.Join(home(), "AppData", "Local", "Steam"))
		// A Steam client or library parked on another fixed drive.
		for c := 'D'; c <= 'Z'; c++ {
			d := string(c) + `:\`
			if _, err := os.Stat(d); err != nil {
				continue
			}
			roots = append(roots,
				d+"SteamLibrary", d+"Steam", d+`Program Files (x86)\Steam`,
				d+`Games\Steam`, d+`Games\SteamLibrary`)
		}
	case "darwin":
		roots = append(roots, filepath.Join(home(), "Library", "Application Support", "Steam"))
	default:
		roots = append(roots,
			filepath.Join(home(), ".local", "share", "Steam"),
			filepath.Join(home(), ".steam", "steam"),
			filepath.Join(home(), ".steam", "root"),
			filepath.Join(home(), ".var", "app", "com.valvesoftware.Steam", ".local", "share", "Steam"),
			filepath.Join(home(), "snap", "steam", "common", ".local", "share", "Steam"))
	}
	libs := append([]string{}, roots...)
	for _, r := range roots {
		if b, err := os.ReadFile(filepath.Join(r, "steamapps", "libraryfolders.vdf")); err == nil {
			libs = append(libs, ParseLibraryFolders(string(b))...)
		}
	}
	return dedupePaths(libs)
}

var regRe = regexp.MustCompile(`(?i)^\s*(SteamPath|InstallPath)\s+REG_SZ\s+(.+?)\s*$`)

func windowsRegistrySteamRoots() []string {
	var out []string
	for _, key := range []string{
		`HKCU\Software\Valve\Steam`,
		`HKLM\SOFTWARE\WOW6432Node\Valve\Steam`,
		`HKLM\SOFTWARE\Valve\Steam`,
	} {
		b, err := exec.Command("reg", "query", key).Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if m := regRe.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
				out = append(out, filepath.FromSlash(m[2]))
			}
		}
	}
	return out
}
