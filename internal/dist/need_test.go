package dist

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// archiveBytes is the size of an item's archive as the site serves it.
func archiveBytes(t *testing.T, it fxItem) int64 {
	t.Helper()
	return int64(len(detArchive(t, it.entries())))
}

// After a first run, a new pack that changes one item and adds another needs only those two downloaded and
// extracted. The unchanged item still counts towards the install.
func TestEstimateNeedCountsOnlyWhatIsNotCached(t *testing.T) {
	tuneRetries(t, time.Minute)
	site := newItemsSite(t)
	cache := t.TempDir()
	a := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "a\n", "data.bin": pseudoBody(51, 8<<10)}}
	b := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "b\n", "data.bin": pseudoBody(53, 4<<10)}}
	site.publish(t, "v1", a, b)
	prepareLogged(t, site, cache, false)

	b2 := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "b2\n", "data.bin": pseudoBody(55, 12<<10)}}
	c := fxItem{wid: "102", folder: "ModC", layer: "base", files: map[string]string{"mod.info": "c\n", "data.bin": pseudoBody(57, 2<<10)}}
	site.publish(t, "v2", a, b2, c)
	m, err := FetchManifest(context.Background(), site.srv.URL+"/manifest.json")
	if err != nil {
		t.Fatal(err)
	}

	need := EstimateNeed(m, cache, "lite", false)
	objB2, objC := archiveBytes(t, b2), archiveBytes(t, c)
	if want := objB2 + objC; need.Fetch != want {
		t.Fatalf("Fetch = %d, want the two objects not cached (%d)", need.Fetch, want)
	}
	if want := max(objB2, objC) + itemBytes(b2) + itemBytes(c); need.Cache != want {
		t.Fatalf("Cache = %d, want the largest object in flight plus the items it unpacks (%d)", need.Cache, want)
	}
	if want := itemBytes(a) + itemBytes(b2) + itemBytes(c); need.Install != want {
		t.Fatalf("Install = %d, want every item the profile needs (%d)", need.Install, want)
	}
}

// A refresh fetches every item again, so nothing counts as cached.
func TestEstimateNeedRefreshCountsEveryItem(t *testing.T) {
	tuneRetries(t, time.Minute)
	site := newItemsSite(t)
	cache := t.TempDir()
	a := fxItem{wid: "100", folder: "ModA", layer: "base", files: map[string]string{"mod.info": "a\n"}}
	b := fxItem{wid: "101", folder: "ModB", layer: "base", files: map[string]string{"mod.info": "b\n", "data.bin": pseudoBody(59, 4<<10)}}
	site.publish(t, "v1", a, b)
	prepareLogged(t, site, cache, false)
	m, err := FetchManifest(context.Background(), site.srv.URL+"/manifest.json")
	if err != nil {
		t.Fatal(err)
	}

	need := EstimateNeed(m, cache, "lite", true)
	objA, objB := archiveBytes(t, a), archiveBytes(t, b)
	if want := objA + objB; need.Fetch != want {
		t.Fatalf("Fetch with refresh = %d, want every object (%d)", need.Fetch, want)
	}
	if want := max(objA, objB) + itemBytes(a) + itemBytes(b); need.Cache != want {
		t.Fatalf("Cache with refresh = %d, want %d", need.Cache, want)
	}
}

// A layer manifest keeps its archives beside the tree it extracts, and the full profile adds the client layer.
func TestEstimateNeedForALayerManifest(t *testing.T) {
	doc, err := json.Marshal(Manifest{
		Schema: 1, PackVersion: "v", GameBuild: "42.22.2", PackURL: "pack.json",
		PackSHA256: sha([]byte("pack")), PackBytes: 4,
		Layers: map[string]Layer{
			"base":   {URL: "base.tar.gz", Bytes: 100, SHA256: sha([]byte("base"))},
			"client": {URL: "client.tar.gz", Bytes: 50, SHA256: sha([]byte("client"))},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := FetchManifest(context.Background(), serveManifest(t, string(doc)))
	if err != nil {
		t.Fatal(err)
	}
	lite := EstimateNeed(m, t.TempDir(), "lite", false)
	if lite != (Need{Fetch: 100, Cache: 200, Install: 100}) {
		t.Fatalf("lite layer need = %+v", lite)
	}
	full := EstimateNeed(m, t.TempDir(), "full", false)
	if full != (Need{Fetch: 150, Cache: 300, Install: 150}) {
		t.Fatalf("full layer need = %+v", full)
	}
}

func TestEstimateNeedOfNoManifestIsZero(t *testing.T) {
	if got := EstimateNeed(nil, t.TempDir(), "lite", false); got != (Need{}) {
		t.Fatalf("nil manifest need = %+v", got)
	}
}
