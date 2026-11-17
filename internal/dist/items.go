package dist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// The items_schema 2 layout under the cache directory:
//
//	objects/<tree_sha256>.tar.gz     an object being downloaded; removed once it is extracted
//	items/<tree_sha256>/             the extracted item (mods/<workshop_id>/...) and its completion marker
//	items/index.json                 the item keys of the current and of the previous manifest
//	src/mods/<workshop_id>/          hard links to one extracted item; this is what pack.Install reads
//	src/.pzpack/folder-digests.json  folder digests the manifest publishes, keyed "<workshop_id>/<folder>"
const (
	completeMarker    = ".complete"
	itemLinkMarker    = ".pzpack-item"
	folderDigestsFile = ".pzpack/folder-digests.json"
	indexFile         = "index.json"
	objectAttempts    = 3
)

// linkFile makes a hard link. Tests replace it to exercise the copy fallback that a filesystem
// without hard links, or a volume that refuses them, forces.
var linkFile = os.Link

// itemIndex records the item keys the cache keeps: those of the current manifest and those of the
// previous one. Every other item and object is deleted once a manifest has been prepared.
type itemIndex struct {
	Schema   int      `json:"schema"`
	Current  []string `json:"current"`
	Previous []string `json:"previous"`
}

// prepareItems is the items_schema 2 path of Prepare. Only the items the profile needs are fetched,
// and an item whose key is already complete is not requested at all. The source tree is brought up
// to date one item at a time, and an item whose link already names its key is left alone.
func prepareItems(ctx context.Context, m *Manifest, cacheDir, profile string, refresh bool, progress Progress) (string, error) {
	itemsDir := filepath.Join(cacheDir, "items")
	objectsDir := filepath.Join(cacheDir, "objects")
	srcDir := filepath.Join(cacheDir, "src")
	for _, d := range []string{itemsDir, objectsDir, srcDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	// A source tree written by the layer path holds whole layers, not item links. It is dropped once,
	// so that no layer file can be taken for an item.
	if _, err := os.Stat(filepath.Join(srcDir, ".pzpack", "set")); err == nil {
		if err := os.RemoveAll(srcDir); err != nil {
			return "", err
		}
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			return "", err
		}
	}

	needed := neededItems(m, profile)
	if len(needed) == 0 {
		return "", fmt.Errorf("manifest lists no base items")
	}
	// An item is fetched when its key is not complete in the cache, or when a refresh asks for every item again.
	fetch := make([]bool, len(needed))
	fetching := &overallProgress{report: progress}
	for i, it := range needed {
		if refresh || !itemComplete(filepath.Join(itemsDir, it.TreeSHA256), it.Object.SHA256) {
			fetch[i] = true
			fetching.total += it.Object.Bytes
			fetching.items++
		}
	}
	for i, it := range needed {
		if !fetch[i] {
			continue
		}
		if refresh {
			removeItem(itemsDir, objectsDir, it.TreeSHA256)
		}
		fetching.start(it.Object.Bytes)
		if err := ensureItem(ctx, objectsDir, itemsDir, it, fetching.object); err != nil {
			return "", fmt.Errorf("%s (%s layer): %w", it.WorkshopID, it.Layer, err)
		}
		fetching.finish()
	}
	for k, it := range needed {
		if err := linkItem(srcDir, itemsDir, it); err != nil {
			return "", fmt.Errorf("link %s: %w", it.WorkshopID, err)
		}
		if progress != nil {
			progress(int64(k+1), int64(len(needed)), fmt.Sprintf("linking mods · %d of %d", k+1, len(needed)))
		}
	}
	if err := pruneLinks(srcDir, needed); err != nil {
		return "", err
	}
	if err := writeFolderDigests(srcDir, m); err != nil {
		return "", err
	}
	if progress != nil {
		progress(0, 0, "finishing · updating cache index")
	}
	if err := commitItems(cacheDir, m); err != nil {
		return "", err
	}
	// The layer archives of the previous path are never read again.
	if err := os.RemoveAll(filepath.Join(cacheDir, "layers")); err != nil {
		return "", err
	}
	return srcDir, nil
}

// neededItems returns the items a profile installs: every base item, and the client items for the
// full profile.
func neededItems(m *Manifest, profile string) []ManifestItem {
	var out []ManifestItem
	for _, it := range m.Items {
		if !(it.Layer == "base" || (it.Layer == "client" && profile == "full")) {
			continue
		}
		if !platformsMatch(it.Platforms, runtime.GOOS, runtime.GOARCH) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// platformsMatch reports whether an item gated by platforms applies to goos/goarch. An empty list
// applies everywhere. An entry is "GOOS/GOARCH" or a bare "GOOS".
func platformsMatch(platforms []string, goos, goarch string) bool {
	if len(platforms) == 0 {
		return true
	}
	for _, p := range platforms {
		if p == goos || p == goos+"/"+goarch {
			return true
		}
	}
	return false
}

// ensureItem makes items/<tree_sha256> hold the extracted item. A complete item is reused as it is,
// and its object is not requested. Otherwise the object is downloaded, resumably and with Download's
// retries, and then extracted. An object whose bytes fail the manifest's sha256 is deleted and
// fetched again, at most objectAttempts times in all: a server that keeps returning other bytes is
// not fixed by asking forever.
func ensureItem(ctx context.Context, objectsDir, itemsDir string, it ManifestItem, progress Progress) error {
	final := filepath.Join(itemsDir, it.TreeSHA256)
	if itemComplete(final, it.Object.SHA256) {
		return nil
	}
	archive := filepath.Join(objectsDir, it.TreeSHA256+".tar.gz")
	layer := Layer{URL: it.Object.URL, Bytes: it.Object.Bytes, SHA256: it.Object.SHA256}
	for attempt := 1; ; attempt++ {
		err := Download(ctx, layer, archive, progress)
		if err == nil {
			break
		}
		if !errors.Is(err, errDigestMismatch) && !errors.Is(err, errSizeMismatch) {
			return err
		}
		if attempt >= objectAttempts {
			return fmt.Errorf("object failed verification on all %d downloads: %w", attempt, err)
		}
		if progress != nil {
			progress(0, 0, fmt.Sprintf("retry %d: object for %s failed verification; downloading it again (attempt %d of %d)",
				attempt, it.WorkshopID, attempt+1, objectAttempts))
		}
	}
	if progress != nil {
		progress(0, 0, phaseExtracting)
	}
	if err := extractItem(archive, final, it); err != nil {
		_ = os.Remove(archive)
		return err
	}
	_ = os.Remove(archive)
	return nil
}

// extractItem unpacks an object into a temporary folder, checks that the item's root is there, marks
// the folder complete and renames it under the item's key. The rename is the last step, so a partial
// extraction is never visible under that key.
func extractItem(archive, final string, it ManifestItem) error {
	tmp := final + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	fail := func(err error) error {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := ExtractTarGz(archive, tmp); err != nil {
		return fail(fmt.Errorf("extract %s: %w", it.WorkshopID, err))
	}
	if fi, err := os.Stat(filepath.Join(tmp, "mods", it.WorkshopID)); err != nil || !fi.IsDir() {
		return fail(fmt.Errorf("the object for %s holds no mods/%s folder", it.WorkshopID, it.WorkshopID))
	}
	if err := os.WriteFile(filepath.Join(tmp, completeMarker), []byte(it.Object.SHA256+"\n"), 0o644); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fail(err)
	}
	return nil
}

// phaseExtracting is the progress label ensureItem reports before it unpacks an object. The overall
// progress adds the item's place to it.
const phaseExtracting = "extracting"

// overallProgress turns the callbacks of one run into one overall measure: the bytes of the objects
// fetched so far plus the part of the object in flight. The measure never moves back, even when an
// object starts again from byte zero, and every object's download is labelled with its place in the run.
type overallProgress struct {
	report   Progress
	total    int64 // bytes of every object the run fetches
	items    int   // objects the run fetches
	finished int64 // bytes of the objects already fetched
	index    int   // place of the object in flight, from 1
	inFlight int64 // bytes of the object in flight
	high     int64 // the highest measure reported so far
}

// start begins the next object, which is bytes long.
func (o *overallProgress) start(bytes int64) {
	o.index++
	o.inFlight = bytes
}

// finish records the object in flight as fetched.
func (o *overallProgress) finish() {
	o.finished += o.inFlight
	o.inFlight = 0
}

// object is the Progress for the object in flight. Download reports "downloading" and its retry notices;
// ensureItem reports phaseExtracting before it unpacks the object. A retry notice keeps its label, so
// that isRetryLabel still recognises it.
func (o *overallProgress) object(done, total int64, label string) {
	if o.report == nil {
		return
	}
	switch label {
	case "downloading":
		m := o.measure(done)
		o.report(m, o.total, fmt.Sprintf("downloading · item %d of %d · %s", o.index, o.items, sizePair(m, o.total)))
	case phaseExtracting:
		// The measure stays the bytes fetched so far, so the extraction step neither moves the bar back
		// nor changes the printer's bucket between items.
		o.report(o.measure(o.inFlight), o.total, fmt.Sprintf("extracting · item %d of %d", o.index, o.items))
	default:
		o.report(o.measure(done), o.total, label)
	}
}

// measure returns the overall bytes when the object in flight has objectDone bytes, kept within
// [high, total].
func (o *overallProgress) measure(objectDone int64) int64 {
	m := o.finished + objectDone
	if m > o.total {
		m = o.total
	}
	if m < o.high {
		m = o.high
	}
	o.high = m
	return m
}

// sizePair formats done and total bytes for a progress label, in gigabytes from 1 GB up and megabytes below.
func sizePair(done, total int64) string {
	if total >= 1e9 {
		return fmt.Sprintf("%.1f of %.1f GB", float64(done)/1e9, float64(total)/1e9)
	}
	return fmt.Sprintf("%.1f of %.1f MB", float64(done)/1e6, float64(total)/1e6)
}

// itemComplete reports whether dir is an extracted item whose marker names the object's sha256.
func itemComplete(dir, objectSHA256 string) bool {
	b, err := os.ReadFile(filepath.Join(dir, completeMarker))
	return err == nil && strings.TrimSpace(string(b)) == objectSHA256
}

// linkItem makes src/mods/<workshop_id> mirror the extracted item with hard links. A link that already
// names the item's key is left alone, so an unchanged item costs no writes. Otherwise the old link is
// removed and a new one built. Where the filesystem refuses hard links, the item is copied instead.
func linkItem(srcDir, itemsDir string, it ManifestItem) error {
	dst := filepath.Join(srcDir, "mods", it.WorkshopID)
	if b, err := os.ReadFile(filepath.Join(dst, itemLinkMarker)); err == nil && strings.TrimSpace(string(b)) == it.TreeSHA256 {
		return nil
	}
	from := filepath.Join(itemsDir, it.TreeSHA256, "mods", it.WorkshopID)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := mirrorTree(from, dst, linkFile); err != nil {
		if rmErr := os.RemoveAll(dst); rmErr != nil {
			return rmErr
		}
		if err := mirrorTree(from, dst, copyFile); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dst, itemLinkMarker), []byte(it.TreeSHA256+"\n"), 0o644)
}

// mirrorTree recreates the folders of src under dst and gives every regular file of src a counterpart
// made by linkFn, which is a hard link or a copy. Anything else in src is refused.
func mirrorTree(src, dst string, linkFn func(from, to string) error) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is neither a file nor a folder", path)
		}
		return linkFn(path, out)
	})
}

// copyFile copies one file and keeps its permission bits, as the installer does.
func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// pruneLinks removes the links of items the profile does not need. A link keeps its files alive, so a
// link left behind for an item that is gone would keep that item's bytes on disk.
func pruneLinks(srcDir string, needed []ManifestItem) error {
	keep := map[string]bool{}
	for _, it := range needed {
		keep[it.WorkshopID] = true
	}
	entries, err := os.ReadDir(filepath.Join(srcDir, "mods"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(srcDir, "mods", e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// writeFolderDigests writes the folder digests the manifest publishes, so that pack.Install can keep an
// unchanged folder without reading its source. The file is removed when no item publishes a digest,
// so a digest never outlives the manifest that gave it.
func writeFolderDigests(srcDir string, m *Manifest) error {
	digests := map[string]string{}
	for _, it := range m.Items {
		for _, f := range it.Folders {
			if f.FolderTreeSHA256 != "" {
				digests[it.WorkshopID+"/"+f.Name] = f.FolderTreeSHA256
			}
		}
	}
	path := filepath.Join(srcDir, filepath.FromSlash(folderDigestsFile))
	if len(digests) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(digests)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileSynced(path, b)
}

// writeFileSynced replaces path with data. The bytes reach the disk before the rename that publishes
// them, so a crash cannot leave an empty file under the final name.
func writeFileSynced(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// commitItems records the keys of the current manifest and of the previous one, then deletes every
// item and object outside those two sets. The previous set moves only when the current set changes,
// so a repeat run keeps the rollback point it already had.
func commitItems(cacheDir string, m *Manifest) error {
	indexPath := filepath.Join(cacheDir, "items", indexFile)
	old := readItemIndex(indexPath)
	current := manifestKeys(m)
	previous := old.Previous
	if !sameKeys(current, old.Current) {
		previous = old.Current
	}
	if previous == nil {
		previous = []string{}
	}
	b, err := json.MarshalIndent(itemIndex{Schema: 1, Current: current, Previous: previous}, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileSynced(indexPath, b); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, k := range current {
		keep[k] = true
	}
	for _, k := range previous {
		keep[k] = true
	}
	return gcItems(cacheDir, keep)
}

// gcItems deletes every item folder and object file whose key is not kept. A partial download of a kept
// key stays, so an interrupted download still resumes.
func gcItems(cacheDir string, keep map[string]bool) error {
	itemsDir := filepath.Join(cacheDir, "items")
	entries, err := os.ReadDir(itemsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.Name() == indexFile || keep[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(itemsDir, e.Name())); err != nil {
			return err
		}
	}
	objectsDir := filepath.Join(cacheDir, "objects")
	entries, err = os.ReadDir(objectsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		key := strings.SplitN(e.Name(), ".", 2)[0]
		if keep[key] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(objectsDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// removeItem forgets an item's extracted folder and its object, for a refresh that must fetch again.
func removeItem(itemsDir, objectsDir, key string) {
	_ = os.RemoveAll(filepath.Join(itemsDir, key))
	_ = os.RemoveAll(filepath.Join(objectsDir, key+".tar.gz"))
	_ = os.RemoveAll(filepath.Join(objectsDir, key+".tar.gz.part"))
	_ = os.RemoveAll(filepath.Join(objectsDir, key+".tar.gz.part.etag"))
}

// manifestKeys returns the tree hashes of every item the manifest lists, sorted.
func manifestKeys(m *Manifest) []string {
	seen := map[string]bool{}
	keys := []string{}
	for _, it := range m.Items {
		if !seen[it.TreeSHA256] {
			seen[it.TreeSHA256] = true
			keys = append(keys, it.TreeSHA256)
		}
	}
	sort.Strings(keys)
	return keys
}

// sameKeys reports whether two sorted key lists are equal.
func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readItemIndex returns the recorded index, or an empty one when none can be read.
func readItemIndex(path string) itemIndex {
	var idx itemIndex
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &idx)
	}
	return idx
}
