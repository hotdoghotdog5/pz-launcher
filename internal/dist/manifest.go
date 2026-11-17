// Package dist downloads a published modpack from object storage (S3 or any
// HTTP host) and turns it into a local source tree the installer can copy from.
package dist

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Layer is one downloadable archive (base or client).
type Layer struct {
	URL    string `json:"url"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Files  int    `json:"files"`
}

// Manifest is the small JSON document published next to the archives.
type Manifest struct {
	Schema      int              `json:"schema"`
	PackVersion string           `json:"pack_version"`
	GameBuild   string           `json:"game_build"`
	GeneratedAt string           `json:"generated_at"`
	PackURL     string           `json:"pack_url"`
	PackSHA256  string           `json:"pack_sha256"`
	PackBytes   int64            `json:"pack_bytes"`
	Layers      map[string]Layer `json:"layers"`

	// ItemsSchema 2 publishes one archive per workshop item beside the layers. Any other value,
	// or none, means the layers are the whole pack.
	ItemsSchema    int            `json:"items_schema,omitempty"`
	ObjectsBaseURL string         `json:"objects_base_url,omitempty"`
	Items          []ManifestItem `json:"items,omitempty"`
}

// ManifestItem is one workshop item under items_schema 2. Its object is an archive of
// mods/<workshop_id>/..., named by the item's tree hash. An item whose content is unchanged keeps
// its key, so its object is never requested again.
type ManifestItem struct {
	WorkshopID string           `json:"workshop_id"`
	Layer      string           `json:"layer"`
	TreeSHA256 string           `json:"tree_sha256"`
	Bytes      int64            `json:"bytes"`
	Folders    []ManifestFolder `json:"folders"`
	Object     ItemObject       `json:"object"`
	// Platforms limits the item to builds whose "GOOS/GOARCH" (or bare "GOOS") matches. Empty means all.
	Platforms []string `json:"platforms,omitempty"`
}

// ManifestFolder is one mod folder an item installs. FolderTreeSHA256 is optional. When it is
// present it is the folder's tree digest, so the installer can keep an unchanged folder without
// reading its source.
type ManifestFolder struct {
	Name             string `json:"name"`
	ModID            string `json:"mod_id"`
	FolderTreeSHA256 string `json:"folder_tree_sha256,omitempty"`
}

// ItemObject locates an item's archive. URL resolves against objects_base_url when the manifest
// names one, and against the manifest URL otherwise.
type ItemObject struct {
	URL    string `json:"url"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Progress receives (done, total, label). total may be 0 when unknown.
type Progress func(done, total int64, label string)

func httpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}

// FetchManifest loads and validates the published manifest.
func FetchManifest(ctx context.Context, rawURL string) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch manifest: HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Schema != 1 {
		return nil, fmt.Errorf("unsupported manifest schema %d", m.Schema)
	}
	itemsPath := m.ItemsSchema == 2
	if m.PackURL == "" || (len(m.Layers) == 0 && !(itemsPath && len(m.Items) > 0)) {
		return nil, fmt.Errorf("manifest is missing pack_url or layers")
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	m.PackURL = resolveURL(base, m.PackURL)
	for k, l := range m.Layers {
		l.URL = resolveURL(base, l.URL)
		m.Layers[k] = l
	}
	if itemsPath {
		if err := m.resolveItems(base); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

func resolveURL(base *url.URL, ref string) string {
	u, err := url.Parse(ref)
	if err != nil || u.IsAbs() {
		return ref
	}
	return base.ResolveReference(u).String()
}

// resolveItems checks every item of an items_schema 2 manifest and resolves each object URL. The
// objects_base_url, when present, is the root the object URLs are relative to. Without it they are
// relative to the manifest URL, as layer URLs are.
func (m *Manifest) resolveItems(base *url.URL) error {
	if len(m.Items) == 0 {
		return fmt.Errorf("manifest declares items_schema 2 but lists no items")
	}
	objBase := base
	if m.ObjectsBaseURL != "" {
		root := m.ObjectsBaseURL
		if !strings.HasSuffix(root, "/") {
			root += "/"
		}
		u, err := url.Parse(root)
		if err != nil {
			return fmt.Errorf("objects_base_url: %w", err)
		}
		objBase = base.ResolveReference(u)
	}
	ids := map[string]bool{}
	trees := map[string]bool{}
	for i := range m.Items {
		it := &m.Items[i]
		if err := it.validate(); err != nil {
			return fmt.Errorf("item %q: %w", it.WorkshopID, err)
		}
		if ids[it.WorkshopID] {
			return fmt.Errorf("item %q is listed twice", it.WorkshopID)
		}
		if trees[it.TreeSHA256] {
			return fmt.Errorf("item %q repeats the content of another item (tree %s)", it.WorkshopID, it.TreeSHA256)
		}
		ids[it.WorkshopID] = true
		trees[it.TreeSHA256] = true
		it.Object.URL = resolveURL(objBase, it.Object.URL)
	}
	return nil
}

// validate rejects an item that could not be installed safely: a name or key that is not a plain
// path segment, a digest that is not a SHA-256, or a missing size. A missing or malformed sha256 is
// an error, never a skipped check.
func (it *ManifestItem) validate() error {
	switch {
	case !safeSegment(it.WorkshopID):
		return fmt.Errorf("workshop_id is not a plain name")
	case it.Layer != "base" && it.Layer != "client":
		return fmt.Errorf("layer %q is neither base nor client", it.Layer)
	case !isSHA256Hex(it.TreeSHA256):
		return fmt.Errorf("tree_sha256 is not 64 lowercase hex digits")
	case it.Bytes <= 0:
		return fmt.Errorf("bytes must be positive")
	case it.Object.URL == "":
		return fmt.Errorf("object url is missing")
	case it.Object.Bytes <= 0:
		return fmt.Errorf("object bytes must be positive")
	case !isSHA256Hex(it.Object.SHA256):
		return fmt.Errorf("object sha256 is missing or is not 64 lowercase hex digits")
	}
	for _, f := range it.Folders {
		if !safeSegment(f.Name) {
			return fmt.Errorf("folder name %q is not a plain name", f.Name)
		}
		if f.FolderTreeSHA256 != "" && !isSHA256Hex(f.FolderTreeSHA256) {
			return fmt.Errorf("folder %s: folder_tree_sha256 is not 64 lowercase hex digits", f.Name)
		}
	}
	return nil
}

// safeSegment reports whether s is one path segment that cannot leave its folder and is not a
// hidden entry, so that it can be joined into a cache path.
func safeSegment(s string) bool {
	if s == "" || strings.HasPrefix(s, ".") || strings.ContainsAny(s, "/\\:\x00") {
		return false
	}
	return filepath.IsLocal(s)
}

// isSHA256Hex reports whether s is a lowercase hexadecimal SHA-256 digest.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// FetchPack downloads only the manifest and pack.json, for --list/--server-ini
// and for dry runs that should not pull the payload.
func FetchPack(ctx context.Context, manifestURL, cacheDir string, refresh bool, progress Progress) (string, *Manifest, error) {
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", nil, err
	}
	if progress != nil {
		progress(0, 0, "fetching manifest")
	}
	m, err := FetchManifest(ctx, manifestURL)
	if err != nil {
		return "", nil, err
	}
	packPath := filepath.Join(cacheDir, "pack.json")
	packLayer := Layer{URL: m.PackURL, Bytes: m.PackBytes, SHA256: m.PackSHA256}
	if refresh {
		os.Remove(packPath)
		os.Remove(packPath + ".ok")
	}
	if err := Download(ctx, packLayer, packPath, progress); err != nil {
		return "", nil, fmt.Errorf("pack.json: %w", err)
	}
	return packPath, m, nil
}

// Prepare downloads the manifest, pack.json and what a profile needs, then extracts it into a
// shared source tree. A manifest with items_schema 2 is prepared item by item (see prepareItems);
// any other manifest uses its layers, as before.
func Prepare(ctx context.Context, manifestURL, cacheDir, profile string, refresh bool, progress Progress) (source, packPath string, m *Manifest, err error) {
	if cacheDir == "" {
		cacheDir = DefaultCacheDir()
	}
	layersDir := filepath.Join(cacheDir, "layers")
	srcDir := filepath.Join(cacheDir, "src")
	if err := os.MkdirAll(layersDir, 0o755); err != nil {
		return "", "", nil, err
	}
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return "", "", nil, err
	}

	packPath, m, err = FetchPack(ctx, manifestURL, cacheDir, refresh, progress)
	if err != nil {
		return "", "", nil, err
	}

	if m.ItemsSchema == 2 {
		srcDir, err := prepareItems(ctx, m, cacheDir, profile, refresh, progress)
		if err != nil {
			return "", "", nil, err
		}
		return srcDir, packPath, m, nil
	}

	needed := []string{"base"}
	if profile == "full" {
		needed = append(needed, "client")
	}

	// Download every needed archive before the source tree is touched, so a failed download leaves
	// the working tree as it was.
	archives := make([]string, 0, len(needed))
	var set strings.Builder
	for _, name := range needed {
		layer, ok := m.Layers[name]
		if !ok {
			return "", "", nil, fmt.Errorf("manifest has no %q layer", name)
		}
		archive := filepath.Join(layersDir, name+".tar.gz")
		if refresh {
			os.Remove(archive)
			os.Remove(archive + ".ok")
		}
		if err := Download(ctx, layer, archive, progress); err != nil {
			return "", "", nil, fmt.Errorf("%s layer: %w", name, err)
		}
		archives = append(archives, archive)
		fmt.Fprintf(&set, "%s %s\n", name, layer.SHA256)
	}

	// The tree is rebuilt whenever the layer set differs from the one it was extracted from, so files
	// a newer pack removed cannot survive into the install.
	marker := filepath.Join(srcDir, ".pzpack", "set")
	if !refresh {
		if b, err := os.ReadFile(marker); err == nil && string(b) == set.String() {
			return srcDir, packPath, m, nil
		}
	}
	if progress != nil {
		progress(0, 0, "extracting "+strings.Join(needed, " + "))
	}
	if err := rebuildSource(srcDir, archives, set.String()); err != nil {
		return "", "", nil, err
	}
	return srcDir, packPath, m, nil
}

// DefaultCacheDir is where archives and the extracted source tree are kept.
func DefaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "pzpack")
}
