// Package pack loads the modpack manifest and describes install plans.
package pack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Folder struct {
	Name  string `json:"name"`
	Src   string `json:"src"`
	ModID string `json:"mod_id"`
}

type Item struct {
	WorkshopID string   `json:"workshop_id"`
	Title      string   `json:"title"`
	Layer      string   `json:"layer"` // base | client
	Folders    []Folder `json:"folders"`
	Bytes      int64    `json:"bytes"`
	TreeSHA256 string   `json:"tree_sha256"`
	ModIDs     []string `json:"mod_ids"`
	Platforms  []string `json:"platforms,omitempty"`
}

type Profile struct {
	Label       string   `json:"label"`
	Layers      []string `json:"layers"`
	ApproxBytes int64    `json:"approx_bytes"`
	StubLayer   string   `json:"stub_layer"`
}

type Pack struct {
	Schema      int                `json:"schema"`
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	GameBuild   string             `json:"game_build"`
	BaseBytes   int64              `json:"base_bytes"`
	ClientBytes int64              `json:"client_bytes"`
	Profiles    map[string]Profile `json:"profiles"`
	LoadOrder   []string           `json:"load_order"`
	Items       []Item             `json:"items"`
}

// Load reads and validates a pack.json. A manifest whose folder names or sources could escape
// the mods folder, collide on a case-insensitive filesystem, or disagree with its load_order is
// refused before anything is installed.
func Load(path string) (*Pack, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Pack
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if p.Schema != 1 {
		return nil, fmt.Errorf("unsupported pack schema %d", p.Schema)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// validate checks the manifest's paths and its load order. Folder names are compared
// case-insensitively on every platform, so one pack installs the same everywhere.
func (p *Pack) validate() error {
	items := map[string]bool{}
	claimed := map[string]string{} // lower-cased folder name -> "<workshop_id>/<name>"
	for _, it := range p.Items {
		if it.WorkshopID == "" {
			return fmt.Errorf("item %q has no workshop_id", it.Title)
		}
		if items[it.WorkshopID] {
			return fmt.Errorf("workshop_id %s appears twice in items", it.WorkshopID)
		}
		items[it.WorkshopID] = true
		for _, pl := range it.Platforms {
			if err := checkPlatform(pl); err != nil {
				return fmt.Errorf("item %s: %w", it.WorkshopID, err)
			}
		}
		for _, f := range it.Folders {
			if err := checkFolderName(f.Name); err != nil {
				return fmt.Errorf("item %s: %w", it.WorkshopID, err)
			}
			if err := checkSrc(f.Src); err != nil {
				return fmt.Errorf("item %s folder %s: %w", it.WorkshopID, f.Name, err)
			}
			key := strings.ToLower(f.Name)
			if other, dup := claimed[key]; dup {
				return fmt.Errorf("folder %q (item %s) collides with %s: names must differ even by case", f.Name, it.WorkshopID, other)
			}
			claimed[key] = it.WorkshopID + "/" + f.Name
		}
	}
	inOrder := map[string]bool{}
	for _, wid := range p.LoadOrder {
		if !items[wid] {
			return fmt.Errorf("load_order names %s, which is not in items", wid)
		}
		inOrder[wid] = true
	}
	for _, it := range p.Items {
		if !inOrder[it.WorkshopID] {
			return fmt.Errorf("item %s is missing from load_order, so its mods would never be enabled", it.WorkshopID)
		}
	}
	return nil
}

// windowsReserved are device names Windows refuses as file or folder names, with or without
// an extension.
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// checkFolderName accepts one plain folder name that is valid on every platform the launcher
// runs on: no separators, no traversal, no reserved or illegal characters.
func checkFolderName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("folder name %q is not a name", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("folder name %q contains a path separator", name)
	case !filepath.IsLocal(name):
		return fmt.Errorf("folder name %q is not a local name", name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return fmt.Errorf("folder name %q ends with a dot or space, which Windows does not allow", name)
	case strings.ContainsAny(name, `<>:"|?*`):
		return fmt.Errorf("folder name %q contains a character Windows does not allow", name)
	case hasControl(name):
		return fmt.Errorf("folder name %q contains a control character", name)
	case windowsReserved[strings.ToUpper(strings.SplitN(name, ".", 2)[0])]:
		return fmt.Errorf("folder name %q is a reserved Windows device name", name)
	}
	return nil
}

// checkSrc accepts a source path relative to the source root, written with forward slashes.
func checkSrc(src string) error {
	local := filepath.FromSlash(src)
	switch {
	case src == "":
		return fmt.Errorf("empty src")
	case strings.Contains(src, `\`) || strings.HasPrefix(src, "/") || filepath.IsAbs(local):
		return fmt.Errorf("src %q must be a relative path written with /", src)
	case !filepath.IsLocal(local):
		return fmt.Errorf("src %q leaves the source folder", src)
	case filepath.Clean(local) == ".":
		return fmt.Errorf("src %q names the source root itself", src)
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// joinUnder joins a manifest-relative path onto root and refuses any result that is not
// strictly inside root. It is the last check before a path is used.
func joinUnder(root, rel string) (string, error) {
	local := filepath.FromSlash(rel)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("refusing path %q: it is not local to %s", rel, root)
	}
	out, err := filepath.Abs(filepath.Join(root, local))
	if err != nil {
		return "", err
	}
	base, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if out == base || !strings.HasPrefix(out, base+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing path %q: it resolves outside %s", rel, root)
	}
	return filepath.Join(root, local), nil
}

// sourcePathChecked is SourcePath with the checks Install applies to every folder.
func sourcePathChecked(root string, f Folder) (string, error) {
	if err := checkSrc(f.Src); err != nil {
		return "", err
	}
	return joinUnder(root, f.Src)
}

// Selected returns the items for a profile that apply to the running build, preserving manifest
// order. An item gated by platforms is skipped on every other build.
func (p *Pack) Selected(profile string) ([]Item, error) {
	return p.selectedFor(profile, runtime.GOOS, runtime.GOARCH)
}

// selectedFor is Selected for an explicit build, so tests can ask about any platform.
func (p *Pack) selectedFor(profile, goos, goarch string) ([]Item, error) {
	pr, ok := p.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", profile)
	}
	layers := map[string]bool{}
	for _, l := range pr.Layers {
		layers[l] = true
	}
	var out []Item
	for _, it := range p.Items {
		if layers[it.Layer] && platformMatches(it.Platforms, goos, goarch) {
			out = append(out, it)
		}
	}
	return out, nil
}

// Plan is the concrete install result for a profile.
type Plan struct {
	Profile string
	Items   []Item
	Folders int
	Bytes   int64
	Stubs   int

	stubLayer string // layer whose folders are written as stubs; set by BuildPlan
}

// BuildPlan returns the install plan for a profile on the running build.
func BuildPlan(p *Pack, profile string) (Plan, error) {
	return buildPlanFor(p, profile, runtime.GOOS, runtime.GOARCH)
}

// buildPlanFor is BuildPlan for an explicit build.
func buildPlanFor(p *Pack, profile, goos, goarch string) (Plan, error) {
	items, err := p.selectedFor(profile, goos, goarch)
	if err != nil {
		return Plan{}, err
	}
	stubLayer := p.Profiles[profile].StubLayer
	pl := Plan{Profile: profile, Items: items, stubLayer: stubLayer}
	for _, it := range items {
		pl.Folders += len(it.Folders)
		if stubLayer != "" && it.Layer == stubLayer {
			pl.Stubs += len(it.Folders)
		} else {
			pl.Bytes += it.Bytes
		}
	}
	return pl, nil
}

// SourcePath resolves a folder source relative to the pack root.
func SourcePath(root string, f Folder) string {
	return filepath.Join(root, filepath.FromSlash(f.Src))
}

// ModIDs returns every mod.info id the plan installs.
func (pl Plan) ModIDs() map[string]string {
	out := map[string]string{}
	for _, it := range pl.Items {
		for _, id := range it.ModIDs {
			out[id] = it.Title
		}
	}
	return out
}

// ModIDsFor returns the mod ids for a profile on the running build, in dependency load order.
func (p *Pack) ModIDsFor(profile string) ([]string, error) {
	return p.modIDsFor(profile, runtime.GOOS, runtime.GOARCH)
}

// modIDsFor is ModIDsFor for an explicit build.
func (p *Pack) modIDsFor(profile, goos, goarch string) ([]string, error) {
	items, err := p.selectedFor(profile, goos, goarch)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, it := range items {
		want[it.WorkshopID] = true
	}
	byID := map[string]Item{}
	for _, it := range p.Items {
		byID[it.WorkshopID] = it
	}
	var out []string
	seen := map[string]bool{}
	for _, wid := range p.LoadOrder {
		if !want[wid] {
			continue
		}
		for _, id := range byID[wid].ModIDs {
			id = strings.TrimPrefix(id, "\\")
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out, nil
}
