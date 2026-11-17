package dist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pzpack/internal/pack"
)

// fxItem is one workshop item of a test pack: a single mod folder and the files it holds.
type fxItem struct {
	wid, folder, layer string
	files              map[string]string // path inside the folder -> contents
	digest             bool              // publish the folder's tree digest in the manifest
	published          string            // when set, the digest the manifest publishes instead
}

// entries returns the archive entries of the item, in the layout a builder writes.
func (it fxItem) entries() map[string]string {
	out := map[string]string{}
	for rel, body := range it.files {
		out["mods/"+it.wid+"/mods/"+it.folder+"/"+rel] = body
	}
	return out
}

// folderDigest is the installer's digest of the folder: SHA-256 over sorted "<path>\t<sha256>\n" lines.
func (it fxItem) folderDigest() string {
	rels := make([]string, 0, len(it.files))
	for rel := range it.files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		sum := sha256.Sum256([]byte(it.files[rel]))
		fmt.Fprintf(h, "%s\t%s\n", rel, hex.EncodeToString(sum[:]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sortedKeys returns the names of m in order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// treeKeyOf names a set of archive entries by content.
func treeKeyOf(entries map[string]string) string {
	h := sha256.New()
	for _, n := range sortedKeys(entries) {
		fmt.Fprintf(h, "%s\x00%s\x00", n, entries[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// detArchive writes entries as a gzip tar with fixed metadata, so the same entries always give the same
// bytes, as a deterministic builder does.
func detArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range sortedKeys(entries) {
		body := entries[n]
		hdr := &tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pseudoBody returns n bytes from a fixed seed. The bytes do not compress, so an archive is large
// enough to be cut and resumed.
func pseudoBody(seed uint64, n int) string {
	b := make([]byte, n)
	x := seed
	for i := range b {
		x = x*6364136223846793005 + 1442695040888963407
		b[i] = byte(x >> 56)
	}
	return string(b)
}

// objectFault scripts the answer to one request for an object. The zero value answers in full.
type objectFault struct {
	status int    // answer with this status and no body
	cutAt  int    // send this many body bytes, then end the response early
	stall  bool   // with cutAt: hold the connection open after the cut instead of ending it
	flip   bool   // send the body with its first byte inverted
	swap   []byte // serve these bytes from now on, under a new validator
}

// rangeObject serves one object with Range and If-Match, answering its requests in script order.
type rangeObject struct {
	mu      sync.Mutex
	body    []byte
	version int
	script  []objectFault
	ranges  []string
	ifMatch []string
}

func (o *rangeObject) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	var f objectFault
	if len(o.script) > 0 {
		f, o.script = o.script[0], o.script[1:]
	}
	if f.swap != nil {
		o.body = f.swap
		o.version++
	}
	body, version := o.body, o.version
	o.ranges = append(o.ranges, r.Header.Get("Range"))
	o.ifMatch = append(o.ifMatch, r.Header.Get("If-Match"))
	o.mu.Unlock()

	if f.status != 0 {
		http.Error(w, "injected fault", f.status)
		return
	}
	etag := fmt.Sprintf(`"v%d"`, version)
	if im := r.Header.Get("If-Match"); im != "" && im != etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	var start int64
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
		start, _ = strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"), 10, 64)
	}
	part := append([]byte(nil), body[start:]...)
	if f.flip && len(part) > 0 {
		part[0] ^= 0xff
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.Itoa(len(part)))
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if f.cutAt > 0 && f.cutAt < len(part) {
		_, _ = w.Write(part[:f.cutAt])
		w.(http.Flusher).Flush()
		if f.stall {
			<-r.Context().Done()
		}
		return
	}
	_, _ = w.Write(part)
}

// seen returns the Range and If-Match headers of every request the object has received.
func (o *rangeObject) seen() (ranges, ifMatch []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.ranges...), append([]string(nil), o.ifMatch...)
}

// itemsSite serves one published pack: its manifest, its pack.json and the objects the manifest names.
// It records the path of every request, so that a test can count what was asked for.
type itemsSite struct {
	srv      *httptest.Server
	mu       sync.Mutex
	manifest []byte
	pack     []byte
	objects  map[string]*rangeObject
	paths    []string
}

func newItemsSite(t *testing.T) *itemsSite {
	t.Helper()
	s := &itemsSite{objects: map[string]*rangeObject{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *itemsSite) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	var doc []byte
	switch r.URL.Path {
	case "/manifest.json":
		doc = s.manifest
	case "/pack.json":
		doc = s.pack
	}
	obj := s.objects[r.URL.Path]
	s.mu.Unlock()
	switch {
	case doc != nil:
		_, _ = w.Write(doc)
	case obj != nil:
		obj.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// setObject serves body at path, answering the requests in script first.
func (s *itemsSite) setObject(path string, body []byte, script ...objectFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[path] = &rangeObject{body: body, version: 1, script: script}
}

// object returns the object served at path.
func (s *itemsSite) object(path string) *rangeObject {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[path]
}

// objectRequests returns the paths of the object requests made since the last reset.
func (s *itemsSite) objectRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, p := range s.paths {
		if strings.HasPrefix(p, "/objects/") {
			out = append(out, p)
		}
	}
	return out
}

// resetRequests forgets the requests recorded so far.
func (s *itemsSite) resetRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = nil
}

// publish makes the site serve a pack made of items: a manifest with items_schema 2 whose objects_base_url
// is the site root, a pack.json describing the same items, and one object per item. It returns the tree
// key of each item by workshop id. Objects are served in full unless a test scripts faults for them.
func (s *itemsSite) publish(t *testing.T, version string, items ...fxItem) map[string]string {
	t.Helper()
	keys := map[string]string{}
	var wids []string
	var packItems []map[string]any
	var mItems []ManifestItem
	for _, it := range items {
		entries := it.entries()
		key := treeKeyOf(entries)
		archive := detArchive(t, entries)
		var size int64
		for _, body := range entries {
			size += int64(len(body))
		}
		keys[it.wid] = key
		wids = append(wids, it.wid)
		s.setObject("/objects/"+key+".tar.gz", archive)
		folder := ManifestFolder{Name: it.folder, ModID: it.folder}
		if it.published != "" {
			folder.FolderTreeSHA256 = it.published
		} else if it.digest {
			folder.FolderTreeSHA256 = it.folderDigest()
		}
		mItems = append(mItems, ManifestItem{
			WorkshopID: it.wid, Layer: it.layer, TreeSHA256: key, Bytes: size,
			Folders: []ManifestFolder{folder},
			Object:  ItemObject{URL: "objects/" + key + ".tar.gz", Bytes: int64(len(archive)), SHA256: sha(archive)},
		})
		packItems = append(packItems, map[string]any{
			"workshop_id": it.wid, "title": it.wid, "layer": it.layer, "bytes": size,
			"tree_sha256": key, "mod_ids": []string{it.folder},
			"folders": []map[string]any{{"name": it.folder, "src": "mods/" + it.wid + "/mods/" + it.folder, "mod_id": it.folder}},
		})
	}
	packDoc, err := json.Marshal(map[string]any{
		"schema": 1, "name": "fixture", "version": version, "game_build": "42.21.0",
		"profiles": map[string]any{
			"full": map[string]any{"label": "Full", "layers": []string{"base", "client"}},
			"lite": map[string]any{"label": "Lite", "layers": []string{"base"}},
		},
		"load_order": wids,
		"items":      packItems,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(Manifest{
		Schema: 1, PackVersion: version, GameBuild: "42.21.0",
		PackURL: "pack.json", PackSHA256: sha(packDoc), PackBytes: int64(len(packDoc)),
		ItemsSchema: 2, ObjectsBaseURL: s.srv.URL + "/", Items: mItems,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.manifest, s.pack = manifest, packDoc
	s.mu.Unlock()
	return keys
}

// prepareWith runs Prepare against the site and returns the source root and the pack.json path.
func prepareWith(s *itemsSite, cache, profile string, refresh bool) (string, string, error) {
	src, packPath, _, err := Prepare(context.Background(), s.srv.URL+"/manifest.json", cache, profile, refresh, nil)
	return src, packPath, err
}

// mustPrepare is prepareWith for a run that must succeed.
func mustPrepare(t *testing.T, s *itemsSite, cache, profile string) (string, string) {
	t.Helper()
	src, packPath, err := prepareWith(s, cache, profile, false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return src, packPath
}

// installPack installs the pack Prepare left in packPath into target, then checks the result.
func installPack(t *testing.T, packPath, src, target, profile string) {
	t.Helper()
	p, err := pack.Load(packPath)
	if err != nil {
		t.Fatal(err)
	}
	pl, err := pack.BuildPlan(p, profile)
	if err != nil {
		t.Fatal(err)
	}
	opt := pack.InstallOptions{Source: src, Target: target}
	if _, _, err := pack.Install(p, pl, opt); err != nil {
		t.Fatal(err)
	}
	if problems := pack.Verify(pl, opt); len(problems) != 0 {
		t.Fatalf("install does not match its source: %v", problems)
	}
}

// mustRead returns the contents of a file.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mustWrite creates a file, and its folders, with the given contents.
func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stamp is the identity and modification time of one entry of a tree.
type stamp struct {
	info os.FileInfo
	mod  time.Time
}

// stampTree records every entry under root.
func stampTree(t *testing.T, root string) map[string]stamp {
	t.Helper()
	out := map[string]stamp{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[rel] = stamp{info: fi, mod: fi.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sameStamps reports whether a tree still holds the same entries: the same objects, not replacements,
// with the same modification times.
func sameStamps(a, b map[string]stamp) bool {
	if len(a) != len(b) {
		return false
	}
	for rel, sa := range a {
		sb, ok := b[rel]
		if !ok || !os.SameFile(sa.info, sb.info) || !sa.mod.Equal(sb.mod) {
			return false
		}
	}
	return true
}

// serveManifest serves doc at /dir/manifest.json and returns that URL.
func serveManifest(t *testing.T, doc string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dir/manifest.json" {
			_, _ = w.Write([]byte(doc))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/dir/manifest.json"
}

// validItem is an item that FetchManifest accepts, with the object URL given.
func validItem(objURL string) ManifestItem {
	return ManifestItem{
		WorkshopID: "100", Layer: "base", TreeSHA256: strings.Repeat("a", 64), Bytes: 10,
		Folders: []ManifestFolder{{Name: "ModA", ModID: "ModA"}},
		Object:  ItemObject{URL: objURL, Bytes: 20, SHA256: strings.Repeat("b", 64)},
	}
}

// itemsManifest encodes an items_schema 2 manifest holding items, with the given objects_base_url.
func itemsManifest(t *testing.T, base string, items ...ManifestItem) string {
	t.Helper()
	b, err := json.Marshal(Manifest{
		Schema: 1, PackVersion: "v", GameBuild: "42.21.0", PackURL: "pack.json",
		PackSHA256: sha([]byte("pack")), PackBytes: 4, ItemsSchema: 2, ObjectsBaseURL: base, Items: items,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestManifestItemsResolveObjectURLs(t *testing.T) {
	cases := []struct {
		name, base, objURL string
		want               func(host string) string
	}{
		{"manifest-relative when objects_base_url is absent", "", "objects/k.tar.gz",
			func(h string) string { return h + "/dir/objects/k.tar.gz" }},
		{"objects_base_url with a trailing slash", "https://cdn.example/pzpack/", "objects/k.tar.gz",
			func(string) string { return "https://cdn.example/pzpack/objects/k.tar.gz" }},
		{"objects_base_url without a trailing slash", "https://cdn.example/pzpack", "objects/k.tar.gz",
			func(string) string { return "https://cdn.example/pzpack/objects/k.tar.gz" }},
		{"relative objects_base_url resolves against the manifest", "/pzpack/", "objects/k.tar.gz",
			func(h string) string { return h + "/pzpack/objects/k.tar.gz" }},
		{"absolute object url is kept", "https://cdn.example/pzpack/", "https://other.example/k.tar.gz",
			func(string) string { return "https://other.example/k.tar.gz" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mURL := serveManifest(t, itemsManifest(t, c.base, validItem(c.objURL)))
			host := strings.TrimSuffix(mURL, "/dir/manifest.json")
			m, err := FetchManifest(context.Background(), mURL)
			if err != nil {
				t.Fatal(err)
			}
			if got := m.Items[0].Object.URL; got != c.want(host) {
				t.Fatalf("object url %q, want %q", got, c.want(host))
			}
		})
	}
}

func TestManifestItemsRejectBadItems(t *testing.T) {
	cases := map[string]func(it *ManifestItem){
		"missing sha256":        func(it *ManifestItem) { it.Object.SHA256 = "" },
		"short sha256":          func(it *ManifestItem) { it.Object.SHA256 = strings.Repeat("b", 63) },
		"uppercase sha256":      func(it *ManifestItem) { it.Object.SHA256 = strings.Repeat("B", 64) },
		"zero object bytes":     func(it *ManifestItem) { it.Object.Bytes = 0 },
		"missing object url":    func(it *ManifestItem) { it.Object.URL = "" },
		"zero item bytes":       func(it *ManifestItem) { it.Bytes = 0 },
		"tree key not a digest": func(it *ManifestItem) { it.TreeSHA256 = "../escape" },
		"workshop id escapes":   func(it *ManifestItem) { it.WorkshopID = "../x" },
		"unknown layer":         func(it *ManifestItem) { it.Layer = "cosmetic" },
		"folder name escapes":   func(it *ManifestItem) { it.Folders[0].Name = "a/b" },
		"bad folder digest":     func(it *ManifestItem) { it.Folders[0].FolderTreeSHA256 = "xyz" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			it := validItem("objects/k.tar.gz")
			mutate(&it)
			if _, err := FetchManifest(context.Background(), serveManifest(t, itemsManifest(t, "", it))); err == nil {
				t.Fatal("FetchManifest accepted the item")
			}
		})
	}
}

func TestManifestItemsRejectDuplicatesAndAcceptValid(t *testing.T) {
	a := validItem("objects/k.tar.gz")
	b := validItem("objects/k2.tar.gz")
	b.WorkshopID, b.TreeSHA256 = "101", strings.Repeat("c", 64)
	sameID := validItem("objects/k3.tar.gz")
	sameID.TreeSHA256 = strings.Repeat("d", 64)
	sameTree := validItem("objects/k4.tar.gz")
	sameTree.WorkshopID = "102"

	if _, err := FetchManifest(context.Background(), serveManifest(t, itemsManifest(t, "", a, b))); err != nil {
		t.Fatalf("two distinct items were rejected: %v", err)
	}
	if _, err := FetchManifest(context.Background(), serveManifest(t, itemsManifest(t, "", a, sameID))); err == nil {
		t.Fatal("a workshop id listed twice was accepted")
	}
	if _, err := FetchManifest(context.Background(), serveManifest(t, itemsManifest(t, "", a, sameTree))); err == nil {
		t.Fatal("two items with the same content were accepted")
	}
	if _, err := FetchManifest(context.Background(), serveManifest(t, itemsManifest(t, ""))); err == nil {
		t.Fatal("items_schema 2 without items or layers was accepted")
	}
}

// Without items_schema 2 the items are ignored, so a layer-only manifest keeps working unchanged.
func TestManifestWithoutItemsSchemaIgnoresItems(t *testing.T) {
	bad := validItem("objects/k.tar.gz")
	bad.Object.SHA256 = ""
	for _, schema := range []int{0, 1} {
		m := Manifest{
			Schema: 1, PackVersion: "v", PackURL: "pack.json", PackSHA256: sha([]byte("pack")), PackBytes: 4,
			ItemsSchema: schema, Items: []ManifestItem{bad},
			Layers: map[string]Layer{"base": {URL: "base.tar.gz", Bytes: 1, SHA256: sha([]byte("x"))}},
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := FetchManifest(context.Background(), serveManifest(t, string(b))); err != nil {
			t.Fatalf("items_schema %d: the layer manifest was rejected: %v", schema, err)
		}
	}
}

func TestItemsFetchSurvivesFaults(t *testing.T) {
	tuneRetries(t, 300*time.Millisecond)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(7, 64<<10)}}
	cases := []struct {
		name     string
		script   []objectFault
		requests int
	}{
		{"connection cut twice, then resumed with Range", []objectFault{{cutAt: 10000}, {cutAt: 20000}}, 3},
		{"503 burst", []objectFault{{status: 503}, {status: 503}, {status: 503}}, 4},
		{"stall past the watchdog", []objectFault{{cutAt: 10000, stall: true}}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			site := newItemsSite(t)
			keys := site.publish(t, "v1", item)
			site.setObject("/objects/"+keys["100"]+".tar.gz", detArchive(t, item.entries()), c.script...)
			src, _, err := prepareWith(site, t.TempDir(), "lite", false)
			if err != nil {
				t.Fatalf("prepare through the faults: %v", err)
			}
			if n := len(site.objectRequests()); n != c.requests {
				t.Fatalf("object requested %d times, want %d", n, c.requests)
			}
			if got := mustRead(t, filepath.Join(src, "mods", "100", "mods", "ModA", "data.bin")); got != item.files["data.bin"] {
				t.Fatal("the fetched item differs from the published one")
			}
		})
	}
}

// The object changes while it is downloaded: the resume is refused with 412 and the download starts again
// from byte zero, and the final bytes must match the manifest.
func TestItemsObjectChangedMidDownloadRestartsFromZero(t *testing.T) {
	tuneRetries(t, 300*time.Millisecond)
	v1 := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(7, 64<<10)}}
	v2 := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(9, 64<<10)}}
	site := newItemsSite(t)
	keys := site.publish(t, "v2", v2)
	path := "/objects/" + keys["100"] + ".tar.gz"
	site.setObject(path, detArchive(t, v1.entries()),
		objectFault{cutAt: 10000}, objectFault{swap: detArchive(t, v2.entries())})
	src, _, err := prepareWith(site, t.TempDir(), "lite", false)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	ranges, ifMatch := site.object(path).seen()
	if len(ranges) != 3 || ranges[0] != "" || ranges[1] != "bytes=10000-" || ranges[2] != "" {
		t.Fatalf("Range headers %q, want [empty, bytes=10000-, empty]", ranges)
	}
	if ifMatch[1] != `"v1"` {
		t.Fatalf("resume sent If-Match %q, want the first response's validator", ifMatch[1])
	}
	if got := mustRead(t, filepath.Join(src, "mods", "100", "mods", "ModA", "data.bin")); got != v2.files["data.bin"] {
		t.Fatal("the item after the restart is not the published one")
	}
}

// A corrupt object is detected by its sha256, deleted and downloaded again.
func TestItemsCorruptObjectIsDeletedAndFetchedAgain(t *testing.T) {
	tuneRetries(t, time.Minute)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(11, 16<<10)}}
	site := newItemsSite(t)
	keys := site.publish(t, "v1", item)
	site.setObject("/objects/"+keys["100"]+".tar.gz", detArchive(t, item.entries()), objectFault{flip: true})
	cache := t.TempDir()
	src, _ := mustPrepare(t, site, cache, "lite")
	if n := len(site.objectRequests()); n != 2 {
		t.Fatalf("object requested %d times, want 2 (one corrupt, one good)", n)
	}
	if _, err := os.Stat(filepath.Join(cache, "objects", keys["100"]+".tar.gz")); !os.IsNotExist(err) {
		t.Fatalf("the archive was left in the cache after extraction: %v", err)
	}
	if got := mustRead(t, filepath.Join(src, "mods", "100", "mods", "ModA", "data.bin")); got != item.files["data.bin"] {
		t.Fatal("the item is not the published one after the retry")
	}
}

// An object that stays corrupt is given up after objectAttempts downloads, with a clear error.
func TestItemsPersistentlyCorruptObjectFails(t *testing.T) {
	tuneRetries(t, time.Minute)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(13, 16<<10)}}
	site := newItemsSite(t)
	keys := site.publish(t, "v1", item)
	site.setObject("/objects/"+keys["100"]+".tar.gz", detArchive(t, item.entries()),
		objectFault{flip: true}, objectFault{flip: true}, objectFault{flip: true})
	cache := t.TempDir()
	_, _, err := prepareWith(site, cache, "lite", false)
	if err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("want a verification failure, got %v", err)
	}
	if n := len(site.objectRequests()); n != objectAttempts {
		t.Fatalf("object requested %d times, want %d", n, objectAttempts)
	}
	if _, err := os.Stat(filepath.Join(cache, "items", keys["100"])); !os.IsNotExist(err) {
		t.Fatal("an unverified item was left under its key")
	}
}

// An item whose key is complete is reused without requesting its object. A forced refresh fetches again.
func TestItemsCompleteItemIsNotRequestedAgain(t *testing.T) {
	tuneRetries(t, time.Minute)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "name=A\n"}}
	site := newItemsSite(t)
	site.publish(t, "v1", item)
	cache := t.TempDir()
	mustPrepare(t, site, cache, "lite")
	if n := len(site.objectRequests()); n != 1 {
		t.Fatalf("first run requested %d objects, want 1", n)
	}
	site.resetRequests()
	mustPrepare(t, site, cache, "lite")
	if n := len(site.objectRequests()); n != 0 {
		t.Fatalf("a complete item was requested again (%d times)", n)
	}
	site.resetRequests()
	if _, _, err := prepareWith(site, cache, "lite", true); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if n := len(site.objectRequests()); n != 1 {
		t.Fatalf("a refresh requested %d objects, want 1", n)
	}
}

// Only the current and the previous manifest keep their items. Older items and their objects are deleted.
func TestItemsGCKeepsCurrentAndPreviousOnly(t *testing.T) {
	tuneRetries(t, time.Minute)
	a1 := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "a1"}}
	a3 := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "a3"}}
	b1 := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "b1"}}
	b2 := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "b2"}}
	site := newItemsSite(t)
	cache := t.TempDir()

	keys1 := site.publish(t, "v1", a1, b1)
	mustPrepare(t, site, cache, "lite")
	keys2 := site.publish(t, "v2", a1, b2)
	mustPrepare(t, site, cache, "lite")
	keys3 := site.publish(t, "v3", a3, b2)
	mustPrepare(t, site, cache, "lite")

	exists := func(key string) bool {
		_, err := os.Stat(filepath.Join(cache, "items", key))
		return err == nil
	}
	if exists(keys1["101"]) {
		t.Fatal("ModB of v1, which neither the current nor the previous manifest holds, survived GC")
	}
	for _, key := range []string{keys3["100"], keys3["101"], keys2["100"], keys2["101"]} {
		if !exists(key) {
			t.Fatalf("item %s of the current or previous manifest was deleted", key)
		}
	}
	idx := readItemIndex(filepath.Join(cache, "items", "index.json"))
	wantCurrent := []string{keys3["100"], keys3["101"]}
	wantPrevious := []string{keys2["100"], keys2["101"]}
	sort.Strings(wantCurrent)
	sort.Strings(wantPrevious)
	if !sameKeys(idx.Current, wantCurrent) || !sameKeys(idx.Previous, wantPrevious) {
		t.Fatalf("index current %v previous %v, want %v and %v", idx.Current, idx.Previous, wantCurrent, wantPrevious)
	}
	if left, _ := os.ReadDir(filepath.Join(cache, "objects")); len(left) != 0 {
		t.Fatalf("object files left in the cache after extraction: %d", len(left))
	}
}

// Without hard links the item is copied: the source files are separate files, and the install still works.
func TestItemsLinkFallsBackToCopyWithoutHardLinks(t *testing.T) {
	tuneRetries(t, time.Minute)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(17, 4096)}}
	site := newItemsSite(t)
	keys := site.publish(t, "v1", item)
	old := linkFile
	linkFile = func(string, string) error { return errors.New("hard links refused") }
	t.Cleanup(func() { linkFile = old })
	cache := t.TempDir()
	src, packPath := mustPrepare(t, site, cache, "lite")

	linked := filepath.Join(src, "mods", "100", "mods", "ModA", "data.bin")
	stored := filepath.Join(cache, "items", keys["100"], "mods", "100", "mods", "ModA", "data.bin")
	ls, err := os.Stat(linked)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := os.Stat(stored)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(ls, ss) {
		t.Fatal("the source file is a hard link although hard links were refused")
	}
	if got := mustRead(t, linked); got != item.files["data.bin"] {
		t.Fatal("the copied source differs from the item")
	}
	installPack(t, packPath, src, filepath.Join(t.TempDir(), "mods"), "lite")
}

// A source tree written by the layer path is dropped once, and the layer archives it used are removed.
func TestItemsReplaceLayerSourceOnce(t *testing.T) {
	tuneRetries(t, time.Minute)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "name=A\n"}}
	site := newItemsSite(t)
	site.publish(t, "v1", item)
	cache := t.TempDir()
	mustWrite(t, filepath.Join(cache, "src", ".pzpack", "set"), "base 00\n")
	mustWrite(t, filepath.Join(cache, "src", "mods", "stale", "old.txt"), "layer file")
	mustWrite(t, filepath.Join(cache, "layers", "base.tar.gz"), "old archive")

	src, _ := mustPrepare(t, site, cache, "lite")
	if _, err := os.Stat(filepath.Join(src, "mods", "stale")); !os.IsNotExist(err) {
		t.Fatal("a file of the layer source survived the switch to items")
	}
	if _, err := os.Stat(filepath.Join(src, ".pzpack", "set")); !os.IsNotExist(err) {
		t.Fatal("the layer marker survived the switch to items")
	}
	if _, err := os.Stat(filepath.Join(cache, "layers")); !os.IsNotExist(err) {
		t.Fatal("the legacy layer archives were kept")
	}
	if got := mustRead(t, filepath.Join(src, "mods", "100", "mods", "ModA", "mod.info")); got != "name=A\n" {
		t.Fatal("the item is missing after the switch")
	}
}

// The folder digests a manifest publishes are written for pack.Install, and removed when none is published.
func TestItemsWritePublishedFolderDigestsOnlyWhenPresent(t *testing.T) {
	tuneRetries(t, time.Minute)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", digest: true, files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(19, 2048)}}
	site := newItemsSite(t)
	site.publish(t, "v1", item)
	cache := t.TempDir()
	src, _ := mustPrepare(t, site, cache, "lite")
	b, err := os.ReadFile(filepath.Join(src, ".pzpack", "folder-digests.json"))
	if err != nil {
		t.Fatalf("no folder digests were written: %v", err)
	}
	var digests map[string]string
	if err := json.Unmarshal(b, &digests); err != nil {
		t.Fatal(err)
	}
	if digests["100/ModA"] != item.folderDigest() {
		t.Fatalf("published digest %q, want %q", digests["100/ModA"], item.folderDigest())
	}

	plain := item
	plain.digest = false
	site.publish(t, "v2", plain)
	mustPrepare(t, site, cache, "lite")
	if _, err := os.Stat(filepath.Join(src, ".pzpack", "folder-digests.json")); !os.IsNotExist(err) {
		t.Fatal("folder digests outlived the manifest that published them")
	}
}
