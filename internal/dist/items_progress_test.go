package dist

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// loggedCall is one progress callback, as the run made it.
type loggedCall struct {
	done, total int64
	label       string
}

// callLog records every progress callback of a run, from any goroutine.
type callLog struct {
	mu    sync.Mutex
	calls []loggedCall
}

func (c *callLog) record(done, total int64, label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, loggedCall{done, total, label})
}

func (c *callLog) snapshot() []loggedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]loggedCall(nil), c.calls...)
}

// prepareLogged runs Prepare against the site, recording every progress callback.
func prepareLogged(t *testing.T, s *itemsSite, cache string, refresh bool) []loggedCall {
	t.Helper()
	var log callLog
	if _, _, _, err := Prepare(context.Background(), s.srv.URL+"/manifest.json", cache, "lite", refresh, log.record); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return log.snapshot()
}

// itemBytes is the size of an item's files, the figure the manifest publishes as the item's bytes.
func itemBytes(it fxItem) int64 {
	var n int64
	for _, body := range it.files {
		n += int64(len(body))
	}
	return n
}

// fetchedLabels returns the calls that report an object being downloaded for an item.
func fetchedLabels(calls []loggedCall) []loggedCall {
	var out []loggedCall
	for _, c := range calls {
		if strings.HasPrefix(c.label, "downloading · item ") {
			out = append(out, c)
		}
	}
	return out
}

// The overall download measure has one fixed total, never moves back, and labels each object with its place.
// Extraction and linking are announced with their counts, and the cache index is announced before it is written.
func TestOverallProgressIsMonotonicAndLabelledPerItem(t *testing.T) {
	tuneRetries(t, time.Minute)
	items := []fxItem{
		{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "name=A\n", "data.bin": pseudoBody(31, 64<<10)}},
		{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "name=B\n", "data.bin": pseudoBody(33, 16<<10)}},
		{wid: "102", folder: "ModC", layer: "base", files: map[string]string{"mod.info": "name=C\n"}},
		{wid: "103", folder: "ModD", layer: "base", files: map[string]string{"mod.info": "name=D\n", "data.bin": pseudoBody(35, 8<<10)}},
	}
	site := newItemsSite(t)
	site.publish(t, "v1", items...)
	var wantTotal int64
	for _, it := range items {
		wantTotal += int64(len(detArchive(t, it.entries())))
	}

	calls := prepareLogged(t, site, t.TempDir(), false)
	fetched := fetchedLabels(calls)
	if len(fetched) == 0 {
		t.Fatal("no download was announced")
	}
	var last int64 = -1
	seen := map[string]bool{}
	for _, c := range fetched {
		if c.total != wantTotal {
			t.Fatalf("total %d in %q, want the fixed %d", c.total, c.label, wantTotal)
		}
		if c.done < last {
			t.Fatalf("overall done went back from %d to %d at %q", last, c.done, c.label)
		}
		last = c.done
		var n int
		if _, err := fmt.Sscanf(c.label, "downloading · item %d of 4", &n); err != nil {
			t.Fatalf("download label %q does not name its item of 4", c.label)
		}
		seen[fmt.Sprint(n)] = true
	}
	if last != wantTotal {
		t.Fatalf("the last download reports %d of %d", last, wantTotal)
	}
	for k := 1; k <= 4; k++ {
		if !seen[fmt.Sprint(k)] {
			t.Fatalf("no download was labelled item %d of 4", k)
		}
	}
	var phases []string
	for _, c := range calls {
		if strings.HasPrefix(c.label, "extracting · item ") || strings.HasPrefix(c.label, "linking mods · ") ||
			c.label == "finishing · updating cache index" {
			phases = append(phases, c.label)
		}
	}
	for k := 1; k <= 4; k++ {
		if !containsLabel(phases, fmt.Sprintf("extracting · item %d of 4", k)) {
			t.Fatalf("item %d of 4 is never extracted in the log: %v", k, phases)
		}
	}
	for k := 1; k <= 4; k++ {
		want := fmt.Sprintf("linking mods · %d of 4", k)
		if !containsLabel(phases, want) {
			t.Fatalf("%q is missing from %v", want, phases)
		}
	}
	if phases[len(phases)-1] != "finishing · updating cache index" {
		t.Fatalf("the cache index is not the last phase: %v", phases)
	}
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// An item whose key is complete is not part of the download: the total covers the changed object alone, and the
// linking still counts every item the profile needs.
func TestCompleteItemIsNotCountedInTheDownload(t *testing.T) {
	tuneRetries(t, time.Minute)
	a := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "name=A\n", "data.bin": pseudoBody(41, 8<<10)}}
	b := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "name=B\n", "data.bin": pseudoBody(43, 8<<10)}}
	site := newItemsSite(t)
	cache := t.TempDir()
	site.publish(t, "v1", a, b)
	prepareLogged(t, site, cache, false)

	b2 := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "name=B\n", "data.bin": pseudoBody(45, 8<<10)}}
	site.publish(t, "v2", a, b2)
	calls := prepareLogged(t, site, cache, false)

	wantTotal := int64(len(detArchive(t, b2.entries())))
	fetched := fetchedLabels(calls)
	if len(fetched) == 0 {
		t.Fatal("the changed item was not downloaded")
	}
	for _, c := range fetched {
		if c.total != wantTotal {
			t.Fatalf("total %d in %q, want the changed object alone (%d)", c.total, c.label, wantTotal)
		}
		if !strings.Contains(c.label, "item 1 of 1") {
			t.Fatalf("download label %q counts the complete item", c.label)
		}
	}
	if last := calls[len(calls)-1]; last.label != "finishing · updating cache index" {
		t.Fatalf("the run did not finish: last call %q", last.label)
	}
	if !containsLabel(labelsOf(calls), "linking mods · 2 of 2") {
		t.Fatalf("linking does not count both items: %v", labelsOf(calls))
	}
}

func labelsOf(calls []loggedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.label)
	}
	return out
}

// With every item complete there is nothing to download, so no download or extraction is announced.
func TestNothingToFetchAnnouncesNoDownload(t *testing.T) {
	tuneRetries(t, time.Minute)
	site := newItemsSite(t)
	cache := t.TempDir()
	site.publish(t, "v1",
		fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "name=A\n"}},
		fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "name=B\n"}})
	prepareLogged(t, site, cache, false)

	calls := prepareLogged(t, site, cache, false)
	for _, c := range calls {
		if strings.HasPrefix(c.label, "downloading · item ") || strings.HasPrefix(c.label, "extracting · item ") {
			t.Fatalf("a complete cache announced %q", c.label)
		}
	}
	if !containsLabel(labelsOf(calls), "finishing · updating cache index") {
		t.Fatalf("the cache index is not announced: %v", labelsOf(calls))
	}
}

// A retry notice keeps its text, so the console printer still recognises it as a retry.
func TestRetryNoticesPassThroughUnchanged(t *testing.T) {
	tuneRetries(t, time.Minute)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(47, 16<<10)}}
	site := newItemsSite(t)
	keys := site.publish(t, "v1", item)
	site.setObject("/objects/"+keys["100"]+".tar.gz", detArchive(t, item.entries()),
		objectFault{status: 503}, objectFault{status: 503})

	calls := prepareLogged(t, site, t.TempDir(), false)
	var retries []loggedCall
	for _, c := range calls {
		if strings.Contains(c.label, "retry") {
			retries = append(retries, c)
		}
	}
	if len(retries) != 2 {
		t.Fatalf("%d retry notices, want 2: %v", len(retries), labelsOf(calls))
	}
	for i, c := range retries {
		if !strings.HasPrefix(c.label, fmt.Sprintf("retry %d: ", i+1)) {
			t.Fatalf("retry notice %d is %q; it must start with its number and keep its text", i+1, c.label)
		}
		if c.total <= 0 {
			t.Fatalf("retry notice %q reports no size", c.label)
		}
	}
}

// A verification failure is a retry notice too: the console must show that the object is being fetched again.
func TestVerificationFailureIsAnnouncedAsARetry(t *testing.T) {
	tuneRetries(t, time.Minute)
	item := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{
		"mod.info": "name=A\n", "data.bin": pseudoBody(49, 16<<10)}}
	site := newItemsSite(t)
	keys := site.publish(t, "v1", item)
	site.setObject("/objects/"+keys["100"]+".tar.gz", detArchive(t, item.entries()), objectFault{flip: true})

	calls := prepareLogged(t, site, t.TempDir(), false)
	for _, c := range calls {
		if strings.Contains(c.label, "failed verification") {
			if !strings.HasPrefix(c.label, "retry 1: object for 100 failed verification; downloading it again (attempt 2 of 3)") {
				t.Fatalf("verification notice is %q", c.label)
			}
			return
		}
	}
	t.Fatalf("no verification notice was announced: %v", labelsOf(calls))
}
