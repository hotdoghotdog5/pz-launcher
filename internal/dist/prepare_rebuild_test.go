package dist

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A file that a newer pack version removed must not survive in the shared source tree and
// get installed (PZL-3 finding 4, observation D1).
func TestPrepareDropsFilesRemovedByNewerPack(t *testing.T) {
	packDoc := []byte(`{"schema":1,"name":"test","version":"1","game_build":"42.21.0","profiles":{},"load_order":[],"items":[]}`)
	v1 := makeTarGz(t, map[string]string{"mods/123/mod.info": "name=A\n", "mods/123/old.lua": "stale\n"})
	v2 := makeTarGz(t, map[string]string{"mods/123/mod.info": "name=A\n"})
	served := v1
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		m := Manifest{
			Schema: 1, PackVersion: "1", GameBuild: "42.21.0",
			PackURL: srv.URL + "/pack.json", PackSHA256: sha(packDoc), PackBytes: int64(len(packDoc)),
			Layers: map[string]Layer{"base": {URL: srv.URL + "/base.tar.gz", Bytes: int64(len(served)), SHA256: sha(served)}},
		}
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/pack.json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(packDoc) })
	mux.HandleFunc("/base.tar.gz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(served) })
	srv = httptest.NewServer(mux)
	defer srv.Close()

	cache := t.TempDir()
	ctx := context.Background()
	src, _, _, err := Prepare(ctx, srv.URL+"/manifest.json", cache, "lite", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "mods", "123", "old.lua")); err != nil {
		t.Fatal("old.lua missing after the first prepare")
	}

	served = v2
	src, _, _, err = Prepare(ctx, srv.URL+"/manifest.json", cache, "lite", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(src, "mods", "123", "old.lua")); err == nil {
		t.Fatal("old.lua was removed by pack 2 but survived in the shared source tree")
	}
}
