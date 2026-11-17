package dist

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeTarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
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

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestPrepareDownloadsAndExtracts(t *testing.T) {
	pack := []byte(`{"schema":1,"name":"test","version":"1","game_build":"42.21.0","profiles":{},"load_order":[],"items":[]}`)
	layer := makeTarGz(t, map[string]string{
		"mods/123/mod.info": "name=Test\nid=TestMod\n",
	})
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		m := Manifest{
			Schema: 1, PackVersion: "1", GameBuild: "42.21.0",
			PackURL: srv.URL + "/pack.json", PackSHA256: sha(pack), PackBytes: int64(len(pack)),
			Layers: map[string]Layer{"base": {URL: srv.URL + "/base.tar.gz", Bytes: int64(len(layer)), SHA256: sha(layer)}},
		}
		json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/pack.json", func(w http.ResponseWriter, r *http.Request) { w.Write(pack) })
	mux.HandleFunc("/base.tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(layer) })
	srv = httptest.NewServer(mux)
	defer srv.Close()

	cache := t.TempDir()
	src, packPath, m, err := Prepare(context.Background(), srv.URL+"/manifest.json", cache, "lite", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.PackVersion != "1" {
		t.Fatalf("version %q", m.PackVersion)
	}
	if _, err := os.Stat(packPath); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(src, "mods", "123", "mod.info"))
	if err != nil || !strings.Contains(string(body), "TestMod") {
		t.Fatalf("extracted file: %q err=%v", body, err)
	}

	// Second run must use the verified cache, not the network.
	srv.Close()
	if _, _, _, err := Prepare(context.Background(), srv.URL+"/manifest.json", cache, "lite", false, nil); err == nil {
		t.Fatal("expected the closed server to fail the manifest fetch")
	}
}

func TestDownloadResumes(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 1000)
	sum := sha(body)
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		if r.Header.Get("Range") == "bytes=5000-" {
			if r.Header.Get("If-Match") != `"v1"` {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", "bytes 5000-9999/10000")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(body[5000:])
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "layer.tar.gz")
	if err := os.WriteFile(dest+".part", body[:5000], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".part.etag", []byte(`"v1"`), 0o644); err != nil {
		t.Fatal(err)
	}
	layer := Layer{URL: srv.URL + "/x", Bytes: int64(len(body)), SHA256: sum}
	if err := Download(context.Background(), layer, dest, nil); err != nil {
		t.Fatal(err)
	}
	if gotRange != "bytes=5000-" {
		t.Fatalf("expected a resume request, got %q", gotRange)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Fatal("content mismatch after resume")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bad.tar.gz")
	if err := os.WriteFile(archive, makeTarGz(t, map[string]string{"../evil": "x"}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarGz(archive, filepath.Join(dir, "out")); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestExtractRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "mods/x/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777})
	tw.Close()
	gz.Close()
	archive := filepath.Join(dir, "bad.tar.gz")
	os.WriteFile(archive, buf.Bytes(), 0o644)
	if err := ExtractTarGz(archive, filepath.Join(dir, "out")); err == nil {
		t.Fatal("expected symlink to be rejected")
	}
}
