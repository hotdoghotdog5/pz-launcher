package pack

import (
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"pzpack/internal/pz"
)

type InstallOptions struct {
	Source   string
	Target   string
	DryRun   bool
	Limit    int
	Progress func(done, total int64, label string)
}

// state is the launcher's record of what it owns under a target folder.
type state struct {
	Version     string            `json:"version"`
	Profile     string            `json:"profile"`
	Folders     []string          `json:"folders"`
	Digests     map[string]string `json:"digests,omitempty"` // folder -> tree digest this launcher last wrote
	InstalledAt string            `json:"installed_at"`
}

const stateDir = ".pzpack"

// keepBackups is how many run folders under .pzpack/backup survive a successful install.
const keepBackups = 5

// freeBytesFn reports free space on the target volume. Tests replace it.
var freeBytesFn = freeBytes

func loadState(target string) state {
	var s state
	b, err := os.ReadFile(filepath.Join(target, stateDir, "state.json"))
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveState(target string, s state) error {
	dir := filepath.Join(target, stateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "state.json.tmp")
	if err := writeSynced(tmp, b); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}

// writeSynced writes data to path and flushes it to disk, so a rename that follows cannot
// publish a file whose contents exist only in the page cache.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
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
	return f.Close()
}

// foldName maps a folder name to the key used to compare names: case-insensitively where
// the filesystem is (APFS and NTFS by default), exactly elsewhere.
func foldName(n string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(n)
	}
	return n
}

// recordedDigest returns the digest recorded for a folder, matching names the way the
// filesystem does.
func recordedDigest(digests map[string]string, name string) (string, bool) {
	if d, ok := digests[name]; ok {
		return d, true
	}
	for k, d := range digests {
		if foldName(k) == foldName(name) {
			return d, true
		}
	}
	return "", false
}

// dropFolded forgets the digest recorded for a folder name.
func dropFolded(digests map[string]string, name string) {
	for k := range digests {
		if foldName(k) == foldName(name) {
			delete(digests, k)
		}
	}
}

// digestsFor returns the recorded digests of the named folders, or nil when there are none.
func digestsFor(names []string, digests map[string]string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		if d, ok := recordedDigest(digests, n); ok {
			out[n] = d
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Install copies the plan into target. A folder joins the launcher's ownership list only after
// this run has taken it over: a user's entry moved aside into a backup, an owned folder removed
// (or backed up when the user changed it), or a name confirmed absent. Owned folders that left
// the profile are removed the same way. A --limit run keeps them.
func Install(p *Pack, pl Plan, opt InstallOptions) (rn int, rb int64, rerr error) {
	if len(pl.Items) == 0 {
		return 0, 0, fmt.Errorf("plan for profile %q is empty; refusing to install (would remove every managed mod)", pl.Profile)
	}
	truncated := opt.Limit > 0 && len(pl.Items) > opt.Limit
	if truncated {
		pl.Items = pl.Items[:opt.Limit]
	}
	stubLayer := p.Profiles[pl.Profile].StubLayer
	if err := checkPlan(pl, stubLayer, opt); err != nil {
		return 0, 0, err
	}
	var total int64
	for _, it := range pl.Items {
		if stubLayer != "" && it.Layer == stubLayer {
			continue
		}
		total += it.Bytes
	}
	if opt.DryRun {
		return len(pl.Items), total, nil
	}
	if err := os.MkdirAll(opt.Target, 0o755); err != nil {
		return 0, 0, err
	}
	if b, err := os.ReadFile(filepath.Join(opt.Target, stateDir, "state.json")); err == nil {
		var chk state
		if jerr := json.Unmarshal(b, &chk); jerr != nil {
			return 0, 0, fmt.Errorf("%s is corrupt (%v); refusing to guess which folders are ours. Fix or delete it", filepath.Join(opt.Target, stateDir, "state.json"), jerr)
		}
	}
	if overlaps(opt.Source, opt.Target) {
		return 0, 0, fmt.Errorf("source %s and target %s overlap; run the launcher from the pack folder, not from the Zomboid mods folder", opt.Source, opt.Target)
	}
	if err := checkSources(pl, stubLayer, opt); err != nil {
		return 0, 0, err
	}
	old := loadState(opt.Target)
	owned := map[string]bool{}
	for _, f := range old.Folders {
		owned[foldName(f)] = true
	}
	published := loadPublishedDigests(opt.Source)
	if err := checkSpace(pl, opt, stubLayer, owned, total); err != nil {
		return 0, 0, err
	}

	digests := map[string]string{}
	for k, v := range old.Digests {
		digests[k] = v
	}
	attempted := []string{}
	newFolders := []string{}
	// A failed run records only what it took over. The next run then keeps a half-copied folder
	// as ours instead of moving it aside, and never owns a folder this run did not touch.
	defer func() {
		if rerr == nil {
			return
		}
		seen := map[string]bool{}
		var all []string
		for _, f := range append(append(append([]string{}, old.Folders...), newFolders...), attempted...) {
			if seen[foldName(f)] {
				continue
			}
			dst, err := joinUnder(opt.Target, f)
			if err != nil {
				continue
			}
			if _, err := os.Lstat(dst); err != nil {
				continue // already gone: nothing of ours is left to own
			}
			seen[foldName(f)] = true
			all = append(all, f)
		}
		sort.Strings(all)
		_ = saveState(opt.Target, state{
			Version: old.Version, Profile: old.Profile, Folders: all,
			Digests: digestsFor(all, digests), InstalledAt: old.InstalledAt,
		})
	}()

	var backupRoot string // created on first use: most runs move nothing aside
	moveAside := func(name, dst string) error {
		if backupRoot == "" {
			base := filepath.Join(opt.Target, stateDir, "backup")
			if err := os.MkdirAll(base, 0o755); err != nil {
				return err
			}
			dir, err := os.MkdirTemp(base, time.Now().Format("20060102-150405.000000000")+"-")
			if err != nil {
				return err
			}
			backupRoot = dir
		}
		return os.Rename(dst, filepath.Join(backupRoot, name))
	}
	// takeOver clears dst so this run can install there. A user's entry moves aside into backup.
	// An owned folder is removed, unless it no longer matches what this launcher wrote, in which
	// case it moves aside first. Either way the digest recorded for the name is forgotten, because
	// the folder is about to change.
	takeOver := func(name, dst string) error {
		want, recorded := recordedDigest(digests, name)
		dropFolded(digests, name)
		fi, err := os.Lstat(dst)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot inspect %s: %w", dst, err)
		}
		if !owned[foldName(name)] {
			if err := moveAside(name, dst); err != nil {
				return fmt.Errorf("backup existing %s: %w", name, err)
			}
			return nil
		}
		if recorded && fi.IsDir() {
			got, err := treeDigest(dst)
			if err != nil {
				return fmt.Errorf("hash owned folder %s: %w", name, err)
			}
			if got != want {
				if err := moveAside(name, dst); err != nil {
					return fmt.Errorf("backup changed %s: %w", name, err)
				}
				return nil
			}
		}
		return os.RemoveAll(dst)
	}

	var done, skipped int64 // done: bytes copied; skipped: bytes of folders kept as they are
	installed := 0
	for _, it := range pl.Items {
		isStub := stubLayer != "" && it.Layer == stubLayer
		for _, f := range it.Folders {
			if opt.Progress != nil {
				opt.Progress(done+skipped, total, installLabel(f.Name))
			}
			dst, err := joinUnder(opt.Target, f.Name)
			if err != nil {
				return installed, done, err
			}
			if isStub {
				if err := takeOver(f.Name, dst); err != nil {
					return installed, done, err
				}
				attempted = append(attempted, f.Name) // dst is ours from here on
				if err := writeStub(dst, f, it.Title); err != nil {
					return installed, done, fmt.Errorf("stub %s: %w", f.Name, err)
				}
				digest, err := treeDigest(dst)
				if err != nil {
					return installed, done, err
				}
				digests[f.Name] = digest
				newFolders = append(newFolders, f.Name)
				installed++
				continue
			}
			src, err := sourcePathChecked(opt.Source, f)
			if err != nil {
				return installed, done, err
			}
			same, err := keepsFolder(it.WorkshopID, f, src, dst, digests, published, owned)
			if err != nil {
				return installed, done, err
			}
			if same {
				// The folder already holds what the plan wants, so it stays as it is and nothing is
				// copied. Its bytes still count towards progress.
				if st, err := statTree(dst); err == nil {
					skipped += st.bytes
				}
				if opt.Progress != nil {
					opt.Progress(done+skipped, total, installLabel(f.Name))
				}
				newFolders = append(newFolders, f.Name)
				installed++
				continue
			}
			if err := takeOver(f.Name, dst); err != nil {
				return installed, done, err
			}
			attempted = append(attempted, f.Name) // dst is ours from here on
			_, digest, err := copyDir(src, dst, func(w int64) {
				done += w
				if opt.Progress != nil {
					opt.Progress(done+skipped, total, installLabel(f.Name))
				}
			})
			if err != nil {
				return installed, done, fmt.Errorf("copy %s: %w", f.Name, err)
			}
			digests[f.Name] = digest
			newFolders = append(newFolders, f.Name)
			installed++
		}
	}

	// Owned folders that left the profile go too. A --limit run keeps them, so a partial install
	// never deletes mods it did not replace.
	keep := map[string]bool{}
	for _, f := range newFolders {
		keep[foldName(f)] = true
	}
	if opt.Progress != nil && !truncated {
		for _, f := range old.Folders {
			if !keep[foldName(f)] {
				opt.Progress(0, 0, "removing folders that left the pack")
				break
			}
		}
	}
	for _, f := range old.Folders {
		if keep[foldName(f)] {
			continue
		}
		if truncated {
			newFolders = append(newFolders, f)
			continue
		}
		dst, err := joinUnder(opt.Target, f)
		if err != nil {
			return installed, done, err
		}
		if err := takeOver(f, dst); err != nil {
			return installed, done, err
		}
	}

	sort.Strings(newFolders)
	if opt.Progress != nil {
		opt.Progress(0, 0, "finishing · saving state")
	}
	if err := saveState(opt.Target, state{
		Version: p.Version, Profile: pl.Profile, Folders: newFolders,
		Digests:     digestsFor(newFolders, digests),
		InstalledAt: time.Now().Format(time.RFC3339),
	}); err != nil {
		return installed, done, err
	}
	pruneBackups(filepath.Join(opt.Target, stateDir, "backup"), keepBackups)
	return installed, done, nil
}

// installLabel is the progress label of one folder being installed. The "installing · " phase keeps the
// console to one line for the whole install, where a label per folder name would print one line each.
func installLabel(folder string) string {
	return "installing · " + folder
}

// checkPlan validates every folder name and source in the plan, and that each path stays inside
// its root. Install calls it before anything is read from disk or changed on it.
func checkPlan(pl Plan, stubLayer string, opt InstallOptions) error {
	for _, it := range pl.Items {
		for _, f := range it.Folders {
			if err := checkFolderName(f.Name); err != nil {
				return fmt.Errorf("item %s: %w", it.WorkshopID, err)
			}
			if _, err := joinUnder(opt.Target, f.Name); err != nil {
				return err
			}
			if stubLayer != "" && it.Layer == stubLayer {
				continue
			}
			if _, err := sourcePathChecked(opt.Source, f); err != nil {
				return fmt.Errorf("item %s folder %s: %w", it.WorkshopID, f.Name, err)
			}
		}
	}
	return nil
}

// checkSources confirms every copied folder exists in the source. It runs before the first
// folder is taken over, so a missing one fails the run without moving anything aside.
func checkSources(pl Plan, stubLayer string, opt InstallOptions) error {
	for _, it := range pl.Items {
		if stubLayer != "" && it.Layer == stubLayer {
			continue
		}
		for _, f := range it.Folders {
			src, err := sourcePathChecked(opt.Source, f)
			if err != nil {
				return err
			}
			if st, err := os.Stat(src); err != nil || !st.IsDir() {
				return fmt.Errorf("missing source folder %s (check --source; expected <source>/mods/<workshop_id>)", src)
			}
		}
	}
	return nil
}

// checkSpace refuses an install the target volume cannot hold. Copying needs about the pack's
// size, and SpaceMarginPct leaves headroom. It was 3x once, but this check runs after the download cache
// has filled the same volume, and 3x then refused installs that had room, after the user had waited
// for the whole download. The preflight in main uses the same rule, so that it cannot accept a run this
// check refuses. Folders moved aside are renames on the same volume, but they are counted as well.
// Where free space cannot be read, the check is skipped.
func checkSpace(pl Plan, opt InstallOptions, stubLayer string, owned map[string]bool, total int64) error {
	var moved int64
	for _, it := range pl.Items {
		if stubLayer != "" && it.Layer == stubLayer {
			continue
		}
		for _, f := range it.Folders {
			if owned[foldName(f.Name)] {
				continue
			}
			dst, err := joinUnder(opt.Target, f.Name)
			if err != nil {
				return err
			}
			if _, err := os.Lstat(dst); err != nil {
				continue
			}
			st, err := statTree(dst)
			if err != nil {
				return err
			}
			moved += st.bytes
		}
	}
	need := InstallSpaceNeed(total) + moved
	free, ok := freeBytesFn(opt.Target)
	if !ok {
		return nil
	}
	if free < uint64(need) {
		return fmt.Errorf("not enough free space on the volume holding %s: need about %s (the %s pack plus %d%% headroom and %s of user folders to move aside), have %s",
			opt.Target, humanBytes(uint64(need)), humanBytes(uint64(total)), SpaceMarginPct, humanBytes(uint64(moved)), humanBytes(free))
	}
	return nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// pruneBackups keeps the newest keepBackups run folders under base and removes the rest. Run
// folder names start with a fixed-width timestamp, so sorting by name sorts by age.
func pruneBackups(base string, keep int) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	var runs []string
	for _, e := range entries {
		if e.IsDir() {
			runs = append(runs, e.Name())
		}
	}
	sort.Strings(runs)
	for i := 0; i < len(runs)-keep; i++ {
		_ = os.RemoveAll(filepath.Join(base, runs[i]))
	}
}

// Uninstall removes every folder the launcher owns.
func Uninstall(target string) error {
	s := loadState(target)
	for _, f := range s.Folders {
		if err := os.RemoveAll(filepath.Join(target, f)); err != nil {
			return err
		}
	}
	// Keep .pzpack/backup: it holds mod folders and default.txt the user had
	// before the launcher touched them.
	_ = os.Remove(filepath.Join(target, stateDir, "state.json"))
	_ = os.Remove(filepath.Join(target, stateDir)) // only succeeds when empty
	return nil
}

// InstalledProfile returns the profile recorded by the last install, or "".
func InstalledProfile(target string) string { return loadState(target).Profile }

func writeStub(dir string, f Folder, title string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	id := f.ModID
	if id == "" {
		id = f.Name
	}
	body := fmt.Sprintf("name=%s\nid=%s\ndescription=Compatibility stub for %s. The server supplies this mod's gameplay; install the Full profile for its client-side content.\n",
		title, id, title)
	return os.WriteFile(filepath.Join(dir, "mod.info"), []byte(body), 0o644)
}

// skipDotted reports whether a path component should be excluded from installs.
//
// Mod folders are not always pristine: tooling and agents write working state
// inside them (a sub-agent left `.pi/board.db` and a planning note inside
// ProjectALifeNPCs, 96 KB). A bare filepath.Walk shipped that to the bucket and
// from there onto every client, so dot-entries are excluded at the copy boundary
// rather than trusted not to exist. Only the *relative* components are examined:
// a legitimate mod can sit under a dotted parent directory. ".." is skipped too
// since a component of ".." begins with a dot.
func skipDotted(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// copyDir copies src into dst, skipping dot-entries. It returns the bytes written and the tree
// digest of the regular files it wrote, which the next run compares against.
func copyDir(src, dst string, progress func(int64)) (int64, string, error) {
	var written int64
	var sums []fileSum
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		// rel == "." is the walk root itself and must never be skipped: it is a
		// dotted component by definition, so testing it unconditionally would
		// SkipDir the whole mod folder and silently install nothing.
		if rel != "." && skipDotted(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		out := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(out, info.Mode().Perm()|0o200)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(out)
			return os.Symlink(link, out)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		tmp := out + ".pzpack-tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm()|0o200)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), in)
		cerr := f.Close()
		if err != nil {
			os.Remove(tmp)
			return err
		}
		if cerr != nil {
			os.Remove(tmp)
			return cerr
		}
		if err := os.Rename(tmp, out); err != nil {
			return err
		}
		sums = append(sums, fileSum{rel: filepath.ToSlash(rel), sum: hex.EncodeToString(h.Sum(nil))})
		written += n
		if progress != nil {
			progress(n)
		}
		return nil
	})
	if err != nil {
		return written, "", err
	}
	return written, digestSums(sums), nil
}

// loadPublishedDigests reads the folder digests the source tree was built with, keyed by
// "<workshop_id>/<folder>". Without the file, or when it does not parse, no digest is published and
// each folder is checked against its own source instead.
func loadPublishedDigests(source string) map[string]string {
	if source == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(source, ".pzpack", "folder-digests.json"))
	if err != nil {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// keepsFolder reports whether dst already holds what the plan wants from src, so that copying it
// again would change nothing. All of these must hold. The folder was ours on the last run and a
// digest was recorded for it then. The digest the plan wants equals the recorded one: the published
// digest when the source tree carries one, else the digest of src itself. The installed tree still
// hashes to the recorded digest, so an edit by the user is backed up rather than kept silently.
func keepsFolder(wid string, f Folder, src, dst string, digests, published map[string]string, owned map[string]bool) (bool, error) {
	if !owned[foldName(f.Name)] {
		return false, nil
	}
	recorded, ok := recordedDigest(digests, f.Name)
	if !ok {
		return false, nil
	}
	want, ok := published[wid+"/"+f.Name]
	if !ok {
		var err error
		if want, err = treeDigest(src); err != nil {
			return false, fmt.Errorf("hash the source of %s: %w", f.Name, err)
		}
	}
	if want != recorded {
		return false, nil
	}
	fi, err := os.Lstat(dst)
	if err != nil || !fi.IsDir() {
		return false, nil
	}
	have, err := treeDigest(dst)
	if err != nil {
		return false, nil // takeOver hashes the same tree and reports the failure
	}
	return have == recorded, nil
}

// Verify compares each folder the plan installs with the source folder it was copied from: the
// same file count and byte total, with the installer's copy filter applied to both sides. Stub
// folders must contain their mod.info. It returns one line per mismatch, and nil when the target
// matches. The plan's own Bytes are publisher totals that include dot-entries the installer never
// copies, so they are not used for equality. When opt.Progress is set it is called once per folder,
// stub folders included, with the folder's place in the run.
func Verify(pl Plan, opt InstallOptions) []string {
	items := pl.Items
	if opt.Limit > 0 && len(items) > opt.Limit {
		items = items[:opt.Limit]
	}
	folders := 0
	for _, it := range items {
		folders += len(it.Folders)
	}
	var problems []string
	n := 0
	for _, it := range items {
		stub := pl.stubLayer != "" && it.Layer == pl.stubLayer
		for _, f := range it.Folders {
			n++
			if opt.Progress != nil {
				opt.Progress(int64(n), int64(folders), fmt.Sprintf("verifying · folder %d of %d", n, folders))
			}
			dst, err := joinUnder(opt.Target, f.Name)
			if err != nil {
				problems = append(problems, f.Name+": "+err.Error())
				continue
			}
			if stub {
				if _, err := os.Stat(filepath.Join(dst, "mod.info")); err != nil {
					problems = append(problems, f.Name+": stub mod.info missing")
				}
				continue
			}
			src, err := sourcePathChecked(opt.Source, f)
			if err != nil {
				problems = append(problems, f.Name+": "+err.Error())
				continue
			}
			want, err := statTree(src)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: source unreadable: %v", f.Name, err))
				continue
			}
			got, err := statTree(dst)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: not installed: %v", f.Name, err))
				continue
			}
			if got != want {
				problems = append(problems, fmt.Sprintf("%s: %d files and %d bytes installed, %d files and %d bytes expected",
					f.Name, got.files, got.bytes, want.files, want.bytes))
			}
		}
	}
	return problems
}

// Conflict describes a mod id that exists outside our managed folders.
type Conflict struct {
	ID     string
	Title  string
	Ours   string
	Other  string
	Source string // workshop | author | local
}

// FindConflicts compares pack mod ids with workshop cache, author workshop and
// unowned local mod folders.
func FindConflicts(p *Pack, pl Plan, target, workshopCache, zomboidDir string) []Conflict {
	ours := pl.ModIDs()
	seen := map[string]Conflict{}
	check := func(m map[string]string, source string) {
		for id, dir := range m {
			title, ok := ours[id]
			if !ok {
				continue
			}
			if strings.HasPrefix(dir, target+string(os.PathSeparator)) {
				continue
			}
			if _, exists := seen[id]; !exists {
				seen[id] = Conflict{ID: id, Title: title, Other: dir, Source: source}
			}
		}
	}
	check(pz.WorkshopModIDs(workshopCache), "workshop")
	check(pz.AuthorWorkshopIDs(zomboidDir), "author")
	// Local mods not managed by us are only conflicts if we do not own that folder.
	local := map[string]string{}
	ownedNames := map[string]bool{}
	for _, f := range loadState(target).Folders {
		ownedNames[f] = true
	}
	entries, _ := os.ReadDir(target)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || ownedNames[e.Name()] {
			continue
		}
		dir := filepath.Join(target, e.Name())
		if id, _, _ := pz.ReadModInfo(dir); id != "" {
			local[id] = dir
		}
	}
	check(local, "local")
	out := make([]Conflict, 0, len(seen))
	for _, c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// overlaps reports whether one path contains the other (case-folded on
// case-insensitive platforms).
func overlaps(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, err1 := filepath.Abs(a)
	bb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return false
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		aa, bb = strings.ToLower(aa), strings.ToLower(bb)
	}
	in := func(x, y string) bool { r, err := filepath.Rel(x, y); return err == nil && !strings.HasPrefix(r, "..") }
	return in(aa, bb) || in(bb, aa)
}
