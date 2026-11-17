package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pzpack/internal/dist"
)

// setClock replaces the progress printer's clock for one test. Each read of the clock moves it by step,
// so a test decides how long a run of callbacks takes.
func setClock(t *testing.T, step time.Duration) *time.Time {
	t.Helper()
	clock := time.Unix(1_000_000, 0)
	prev := now
	now = func() time.Time {
		clock = clock.Add(step)
		return clock
	}
	t.Cleanup(func() { now = prev })
	return &clock
}

// stampedWriter keeps what a progress printer wrote, and the clock reading at each write, so that a
// test can measure the silent gaps between lines.
type stampedWriter struct {
	buf    bytes.Buffer
	clock  *time.Time
	stamps []time.Time
}

func (s *stampedWriter) Write(p []byte) (int, error) {
	s.stamps = append(s.stamps, *s.clock)
	return s.buf.Write(p)
}

// longestGap returns the longest stretch between start, each line and end.
func (s *stampedWriter) longestGap(start, end time.Time) time.Duration {
	prev := start
	var longest time.Duration
	for _, at := range s.stamps {
		if gap := at.Sub(prev); gap > longest {
			longest = gap
		}
		prev = at
	}
	if gap := end.Sub(prev); gap > longest {
		longest = gap
	}
	return longest
}

func TestPhaseOfNamesTheStepNotItsCounts(t *testing.T) {
	cases := map[string]string{
		"downloading": "downloading",
		"downloading · item 3 of 9 · 1.0 of 2.0 MB": "downloading",
		"linking mods · 5 of 9":                     "linking mods",
		"extracting · item 1 of 9":                  "extracting",
		"verifying · folder 12 of 356":              "verifying",
		"retry 2: no response for 60s":              "retry",
		"finishing · updating cache index":          "finishing",
		"fetching manifest":                         "fetching manifest",
		"installing · ProjectALife2":                "installing",
		"extracting base + client":                  "extracting base + client",
		"":                                          "",
	}
	for label, want := range cases {
		if got := phaseOf(label); got != want {
			t.Errorf("phaseOf(%q) = %q, want %q", label, got, want)
		}
	}
}

// A new phase prints once, and a phase that comes back does not print again: a per-item download and
// extraction alternate, and a line per switch would flood the console. Bucket lines still print.
func TestPrinterAnnouncesEachPhaseOnceAndKeepsBuckets(t *testing.T) {
	setClock(t, time.Millisecond)
	var out bytes.Buffer
	progress := progressPrinter(&out, 10)
	progress(50, 100, "downloading · item 1 of 2 · 0.0 of 0.0 MB") // new phase, bucket 5: prints
	progress(51, 100, "downloading · item 1 of 2 · 0.0 of 0.0 MB") // same bucket, phase seen: silent
	progress(55, 100, "linking mods · 1 of 2")                     // new phase inside the bucket: prints
	progress(56, 100, "linking mods · 2 of 2")                     // same phase and bucket: silent
	progress(57, 100, "downloading · item 2 of 2")                 // a phase that comes back: silent
	progress(0, 0, "finishing · updating cache index")             // new phase, no size: label alone
	progress(0, 0, "finishing · updating cache index")             // silent
	want := "   50%  downloading · item 1 of 2 · 0.0 of 0.0 MB\n" +
		"   55%  linking mods · 1 of 2\n" +
		"        finishing · updating cache index\n"
	if got := out.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
}

// Retry notices always print on their own line, even when the bucket has not moved.
func TestPrinterRetryLinesAlwaysPrint(t *testing.T) {
	setClock(t, time.Millisecond)
	var out bytes.Buffer
	progress := progressPrinter(&out, 10)
	progress(40, 100, "downloading")
	for i := 1; i <= 4; i++ {
		progress(40, 100, fmt.Sprintf("retry %d: connection interrupted at 40 bytes; resuming", i))
	}
	if n := strings.Count(out.String(), "retry "); n != 4 {
		t.Fatalf("%d retry lines printed for four retries:\n%s", n, out.String())
	}
}

// With no bucket and no new phase, a line still prints once every heartbeatEvery of silence, so a
// stalled-looking run keeps showing that it is alive.
func TestPrinterHeartbeatBoundsSilence(t *testing.T) {
	clock := setClock(t, time.Second)
	start := *clock
	w := &stampedWriter{clock: clock}
	progress := progressPrinter(w, 5)
	for i := 0; i < 120; i++ {
		progress(1, 1000, "downloading · item 1 of 1 · 0.0 of 0.0 MB") // same bucket, same phase
	}
	if want := int(120 * time.Second / heartbeatEvery); len(w.stamps) < want {
		t.Fatalf("%d lines in 120 s of callbacks, want at least %d: a heartbeat every %s", len(w.stamps), want, heartbeatEvery)
	}
	if longest := w.longestGap(start, *clock); longest > heartbeatEvery {
		t.Fatalf("silent for %s, want at most %s", longest, heartbeatEvery)
	}
}

// hexSHA256 returns the lowercase hex SHA-256 of b.
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fixtureTarGz returns a gzip tar of files with fixed metadata, so the same files always give the same bytes.
func fixtureTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		body := files[n]
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
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

// servedPack is a published pack served from a loopback test server.
type servedPack struct {
	url     string       // the manifest URL
	objects atomic.Int64 // object requests the server has answered
}

// serveItemsPack publishes n single-folder base items as an items_schema 2 pack. The last big items also
// carry a bigBytes data file, so the run has a few large objects among many small ones. The object
// bytes are fixed by the item number, so the same call always serves the same pack.
func serveItemsPack(t *testing.T, n, big, bigBytes int) *servedPack {
	t.Helper()
	sp := &servedPack{}
	objects := map[string][]byte{}
	var manifestItems []dist.ManifestItem
	var packItems []map[string]any
	var wids []string
	seed := uint64(0x9e3779b97f4a7c15)
	for i := 0; i < n; i++ {
		wid := strconv.Itoa(100000 + i)
		folder := fmt.Sprintf("Mod%d", i)
		src := "mods/" + wid + "/mods/" + folder
		files := map[string][]byte{src + "/mod.info": []byte(fmt.Sprintf("name=%s\nid=%s\n", folder, folder))}
		if i >= n-big {
			data := make([]byte, bigBytes)
			for j := range data {
				seed ^= seed << 13
				seed ^= seed >> 7
				seed ^= seed << 17
				data[j] = byte(seed)
			}
			files[src+"/data.bin"] = data
		}
		var size int64
		for _, b := range files {
			size += int64(len(b))
		}
		archive := fixtureTarGz(t, files)
		key := hexSHA256(archive)
		objects["/objects/"+key+".tar.gz"] = archive
		wids = append(wids, wid)
		manifestItems = append(manifestItems, dist.ManifestItem{
			WorkshopID: wid, Layer: "base", TreeSHA256: key, Bytes: size,
			Folders: []dist.ManifestFolder{{Name: folder, ModID: folder}},
			Object:  dist.ItemObject{URL: "objects/" + key + ".tar.gz", Bytes: int64(len(archive)), SHA256: key},
		})
		packItems = append(packItems, map[string]any{
			"workshop_id": wid, "title": folder, "layer": "base", "bytes": size,
			"tree_sha256": key, "mod_ids": []string{folder},
			"folders": []map[string]any{{"name": folder, "src": src, "mod_id": folder}},
		})
	}
	packDoc, err := json.Marshal(map[string]any{
		"schema": 1, "name": "fixture", "version": "test", "game_build": "42.22.2",
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
	var mu sync.Mutex
	docs := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/objects/") {
			sp.objects.Add(1)
		}
		mu.Lock()
		body, ok := docs[r.URL.Path]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	manifest, err := json.Marshal(dist.Manifest{
		Schema: 1, PackVersion: "test", GameBuild: "42.22.2",
		PackURL: "pack.json", PackSHA256: hexSHA256(packDoc), PackBytes: int64(len(packDoc)),
		ItemsSchema: 2, ObjectsBaseURL: srv.URL + "/", Items: manifestItems,
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	for path, body := range objects {
		docs[path] = body
	}
	docs["/pack.json"] = packDoc
	docs["/manifest.json"] = manifest
	mu.Unlock()
	sp.url = srv.URL + "/manifest.json"
	return sp
}

// indexOfLine returns the first line that contains s, or -1.
func indexOfLine(lines []string, s string) int {
	for i, l := range lines {
		if strings.Contains(l, s) {
			return i
		}
	}
	return -1
}

// pctOf returns the percentage of a line printed as "  NN%  label", and false for a label-only line.
func pctOf(line string) (int, bool) {
	if len(line) < 6 || line[5] != '%' {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[:5]))
	return n, err == nil
}

// A real Prepare over 309 items, with the console printer as its callback and a clock that moves 50 ms per
// callback. The console must show each phase, in run order, in tens of lines, with no silent stretch longer
// than heartbeatEvery and no percentage that moves back.
func TestPrepareTranscriptFor309ItemsStaysReadable(t *testing.T) {
	sp := serveItemsPack(t, 309, 3, 256<<10)
	clock := setClock(t, 50*time.Millisecond)
	start := *clock
	w := &stampedWriter{clock: clock}
	cache := t.TempDir()
	src, _, _, err := dist.Prepare(context.Background(), sp.url, cache, "lite", false, progressPrinter(w, 5))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := sp.objects.Load(); got != 309 {
		t.Fatalf("%d objects requested, want 309", got)
	}
	if _, err := os.Stat(filepath.Join(src, "mods", "100308", "mods", "Mod308", "data.bin")); err != nil {
		t.Fatalf("the last item was not extracted: %v", err)
	}
	transcript := w.buf.String()
	lines := strings.Split(strings.TrimSuffix(transcript, "\n"), "\n")
	if n := len(lines); n < 20 || n > 150 {
		t.Fatalf("%d lines for 309 items, want on the order of tens:\n%s", n, transcript)
	}
	order := []int{
		indexOfLine(lines, "fetching manifest"),
		indexOfLine(lines, "extracting · item"),
		indexOfLine(lines, "linking mods"),
		indexOfLine(lines, "finishing · updating cache index"),
	}
	for i, at := range order {
		if at < 0 {
			t.Fatalf("phase %d is never announced:\n%s", i, transcript)
		}
		if i > 0 && at <= order[i-1] {
			t.Fatalf("phases out of order: %v\n%s", order, transcript)
		}
	}
	if strings.Contains(transcript, "retry") {
		t.Fatalf("a clean run printed a retry:\n%s", transcript)
	}
	last := -1
	for _, l := range lines {
		if !strings.Contains(l, "downloading · item") {
			continue
		}
		pct, ok := pctOf(l)
		if !ok {
			t.Fatalf("download line without a percentage: %q", l)
		}
		if pct < last {
			t.Fatalf("download percentage moved back from %d to %d:\n%s", last, pct, transcript)
		}
		last = pct
	}
	if longest := w.longestGap(start, *clock); longest > heartbeatEvery {
		t.Fatalf("silent for %s, want at most %s:\n%s", longest, heartbeatEvery, transcript)
	}
}
