package pz

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// vpFixture is one entry of a synthetic Viewpoint.jar.
type vpFixture struct {
	name   string
	data   []byte
	method uint16
	attrs  uint32
}

// vpFixtureEntries is a jar in the shape the shim expects. The entries that need changes contain the
// exact anchors; the unrelated entries show that untouched data survives the rewrite.
func vpFixtureEntries() []vpFixture {
	taaFrag := "#version 330\n" +
		"void main() {\n" +
		"  vec4 a = texelFetch(uCurrent, clamp(at + OFFSETS[i], ivec2(0), size - 1), 0);\n" +
		"  vec4 b = texelFetch(uHistory, h + OFFSETS[i], 0);\n" +
		"  vec4 c = texelFetch(uHistoryCount, h, 0);\n" +
		"}\n"
	const file = 0o100644 << 16
	return []vpFixture{
		{"viewpoint/platform/Gl.class", append(append([]byte("gl-head"), vpGLCall...), []byte("gl-tail")...), zip.Store, file},
		{"viewpoint/platform/ImGuiFrame.class", []byte("imgui-head #version 130 imgui-tail"), zip.Deflate, file},
		{"viewpoint/shaders/taa_out.frag", []byte("// rcas\n" + vpTaaOutOld + "\n"), zip.Deflate, file},
		{"viewpoint/shaderpacks/normal/pack.properties", []byte("a=1\noption.antialiasing.default=taaPlus\nb=2\n"), zip.Deflate, file},
		{"viewpoint/shaderpacks/library/pack.properties", []byte("option.antialiasing.default=taaPlus\n"), zip.Deflate, file},
		{"viewpoint/shaders/taa.frag", []byte(taaFrag), zip.Deflate, file},
		{"viewpoint/platform/", nil, zip.Store, 0o40755 << 16},
		{"viewpoint/shaderpacks/unrelated.txt", []byte("keep me exactly\n"), zip.Deflate, file},
	}
}

// vpSetData returns a copy of entries with the named entry's bytes replaced.
func vpSetData(entries []vpFixture, name string, data []byte) []vpFixture {
	out := make([]vpFixture, len(entries))
	copy(out, entries)
	for i := range out {
		if out[i].name == name {
			out[i].data = data
		}
	}
	return out
}

// vpDropEntry returns a copy of entries without the named entry.
func vpDropEntry(entries []vpFixture, name string) []vpFixture {
	var out []vpFixture
	for _, e := range entries {
		if e.name != name {
			out = append(out, e)
		}
	}
	return out
}

// vpBuildJar writes entries as a zip archive. Each entry gets its own DOS date and time, so a rewrite
// that mixed up timestamps would show.
func vpBuildJar(t *testing.T, entries []vpFixture) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:          e.name,
			Method:        e.method,
			ModifiedDate:  uint16(0x5a00 + i),
			ModifiedTime:  uint16(0x6000 + 3*i),
			ExternalAttrs: e.attrs,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// vpJarOf is the Viewpoint.jar path inside a mods folder.
func vpJarOf(mods string) string {
	return filepath.Join(mods, "Viewpoint", "42", "media", "java", "client", "Viewpoint.jar")
}

// vpModsWith writes jar as Viewpoint.jar under a new mods folder and returns that folder.
func vpModsWith(t *testing.T, jar []byte) string {
	t.Helper()
	mods := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(vpJarOf(mods)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vpJarOf(mods), jar, 0o644); err != nil {
		t.Fatal(err)
	}
	return mods
}

// vpJarEntry is the metadata and content digest of one entry, as the rewrite must preserve them.
type vpJarEntry struct {
	name   string
	method uint16
	date   uint16
	tm     uint16
	attrs  uint32
	sum    [sha256.Size]byte
}

// vpReadJar lists a jar's entries in order, with their metadata and a digest of their bytes.
func vpReadJar(t *testing.T, path string) []vpJarEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	out := make([]vpJarEntry, 0, len(zr.File))
	for _, f := range zr.File {
		data, err := vpReadEntry(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, vpJarEntry{f.Name, f.Method, f.ModifiedDate, f.ModifiedTime, f.ExternalAttrs, sha256.Sum256(data)})
	}
	return out
}

// vpJarData returns one named entry's bytes from a jar.
func vpJarData(t *testing.T, path, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == name {
			data, err := vpReadEntry(f)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
	}
	t.Fatalf("%s has no entry %s", path, name)
	return nil
}

// vpDirNames lists the file names in dir, to check that no temp or backup file was left behind.
func vpDirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// TestMacViewpointShimPatchesFixtureJar applies the shim to a synthetic jar and checks each rewrite,
// the changed-entry list, and that every entry keeps its name, order, timestamp, attributes and method.
func TestMacViewpointShimPatchesFixtureJar(t *testing.T) {
	src := vpFixtureEntries()
	mods := vpModsWith(t, vpBuildJar(t, src))
	changed, err := applyMacViewpointPatches(mods, "darwin", "arm64")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{
		"viewpoint/platform/Gl.class",
		"viewpoint/platform/ImGuiFrame.class",
		"viewpoint/shaders/taa_out.frag",
		"viewpoint/shaderpacks/normal/pack.properties",
		"viewpoint/shaderpacks/library/pack.properties",
		"viewpoint/shaders/taa.frag",
	}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v\nwant    %v", changed, want)
	}

	got := vpReadJar(t, vpJarOf(mods))
	if len(got) != len(src) {
		t.Fatalf("entries = %d, want %d", len(got), len(src))
	}
	for i, e := range got {
		if e.name != src[i].name {
			t.Fatalf("entry %d is %q, want %q (order changed)", i, e.name, src[i].name)
		}
		if e.date != uint16(0x5a00+i) || e.tm != uint16(0x6000+3*i) || e.attrs != src[i].attrs {
			t.Errorf("%s: DOS timestamp or external attributes changed", e.name)
		}
		if !strings.HasSuffix(e.name, "/") && e.method != src[i].method {
			t.Errorf("%s: compression method changed from %d to %d", e.name, src[i].method, e.method)
		}
	}

	jar := vpJarOf(mods)
	wantGL := append(append([]byte("gl-head"), vpGLSkip...), []byte("gl-tail")...)
	if gl := vpJarData(t, jar, vpGLClass); !bytes.Equal(gl, wantGL) {
		t.Errorf("Gl.class = % x, want % x", gl, wantGL)
	}
	im := vpJarData(t, jar, vpImGuiClass)
	if bytes.Contains(im, []byte(vpVer130)) || !bytes.Contains(im, []byte(vpVer330)) {
		t.Errorf("ImGuiFrame.class is not the #version 330 shader: %q", im)
	}
	taaOut := string(vpJarData(t, jar, vpSentinel))
	if strings.Contains(taaOut, vpTexelFetch) || !strings.Contains(taaOut, "c = rcas(texture(uColor, vUv).rgb") {
		t.Errorf("taa_out.frag not rewritten: %q", taaOut)
	}
	taa := string(vpJarData(t, jar, "viewpoint/shaders/taa.frag"))
	for _, s := range []string{
		"vec4 vpFetch(sampler2D t, ivec2 c) {",
		"vpFetch(uCurrent, at + OFFSETS[i])",
		"vpFetch(uHistory, h + OFFSETS[i])",
		"vpFetch(uHistoryCount, h)",
	} {
		if !strings.Contains(taa, s) {
			t.Errorf("taa.frag missing %q", s)
		}
	}
	if strings.Contains(taa, vpTexelFetch) {
		t.Errorf("taa.frag still has texelFetch: %q", taa)
	}
	for _, name := range []string{"viewpoint/shaderpacks/normal/pack.properties", "viewpoint/shaderpacks/library/pack.properties"} {
		p := string(vpJarData(t, jar, name))
		if strings.Contains(p, vpTaaPlus) || !strings.Contains(p, vpFxaa) {
			t.Errorf("%s not rewritten to fxaa: %q", name, p)
		}
	}
	if got := vpJarData(t, jar, "viewpoint/shaderpacks/unrelated.txt"); string(got) != "keep me exactly\n" {
		t.Errorf("unrelated entry changed: %q", got)
	}
}

// TestMacViewpointShimIsIdempotent: a second run on a patched jar changes nothing, and the jar's bytes
// stay the same.
func TestMacViewpointShimIsIdempotent(t *testing.T) {
	mods := vpModsWith(t, vpBuildJar(t, vpFixtureEntries()))
	if _, err := applyMacViewpointPatches(mods, "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(vpJarOf(mods))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := applyMacViewpointPatches(mods, "darwin", "arm64")
	if err != nil || changed != nil {
		t.Fatalf("second run = %v, %v; want nil, nil", changed, err)
	}
	second, err := os.ReadFile(vpJarOf(mods))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("second run rewrote an already-patched jar")
	}
}

// TestMacViewpointShimRefusesWithoutWriting: every refusal the scripts make (a count mismatch, a moved
// anchor, a missing required entry, a non-UTF-8 shader) returns an error and leaves the jar, its backup
// and the folder exactly as they were.
func TestMacViewpointShimRefusesWithoutWriting(t *testing.T) {
	base := vpFixtureEntries()
	cases := []struct {
		name    string
		entries []vpFixture
		want    string
	}{
		{"two glPatchParameteri calls", vpSetData(base, vpGLClass,
			append(append(append([]byte("x"), vpGLCall...), vpGLCall...), []byte("y")...)), "found 2"},
		{"no glPatchParameteri call and no skip", vpSetData(base, vpGLClass, []byte("gl-head only")), "found 0"},
		{"two #version 130 headers", vpSetData(base, vpImGuiClass, []byte("#version 130 and #version 130")), "found 2"},
		{"no #version 130 and no #version 330", vpSetData(base, vpImGuiClass, []byte("nothing here")), "found 0"},
		{"taa.frag texelFetch moved", vpSetData(base, "viewpoint/shaders/taa.frag",
			[]byte("void main() {\ntexelFetch(uCurrent, clamp(at + OFFSETS[i], ivec2(0), size - 1), 0)\n"+
				"texelFetch(uHistory, h + OFFSETS[i], 0)\ntexelFetch(uHistoryCount, h, 0)\ntexelFetch(other, p, 0)\n")), "still present"},
		{"taa.frag anchor changed", vpSetData(base, "viewpoint/shaders/taa.frag",
			[]byte("void main() {\ntexelFetch(uCurrent, p, 0)\n")), "anchor not found"},
		{"taa_out.frag anchor changed", vpSetData(base, vpSentinel,
			[]byte("texelFetch(uColor, p, 0)\n")), "anchor not found"},
		{"shader not UTF-8", vpSetData(base, "viewpoint/shaders/taa.frag", []byte{0xff, 0xfe, 'v', 'o', 'i', 'd'}), "not UTF-8"},
		{"missing Gl.class", vpDropEntry(base, vpGLClass), "is missing"},
		{"missing taa_out.frag sentinel", vpDropEntry(base, vpSentinel), "is missing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mods := vpModsWith(t, vpBuildJar(t, c.entries))
			before, err := os.ReadFile(vpJarOf(mods))
			if err != nil {
				t.Fatal(err)
			}
			changed, err := applyMacViewpointPatches(mods, "darwin", "arm64")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("apply = %v, %v; want an error containing %q", changed, err, c.want)
			}
			after, err := os.ReadFile(vpJarOf(mods))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("a refused patch changed the jar")
			}
			if names := vpDirNames(t, filepath.Dir(vpJarOf(mods))); !reflect.DeepEqual(names, []string{"Viewpoint.jar"}) {
				t.Fatalf("a refused patch left files behind: %v", names)
			}
		})
	}
}

// TestMacViewpointShimToleratesMissingPackProperties: as the present script does, an entry the jar
// does not contain is skipped, and the rest of the patch still applies.
func TestMacViewpointShimToleratesMissingPackProperties(t *testing.T) {
	entries := vpDropEntry(vpDropEntry(vpFixtureEntries(), "viewpoint/shaderpacks/normal/pack.properties"),
		"viewpoint/shaderpacks/library/pack.properties")
	mods := vpModsWith(t, vpBuildJar(t, entries))
	changed, err := applyMacViewpointPatches(mods, "darwin", "arm64")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{"viewpoint/platform/Gl.class", "viewpoint/platform/ImGuiFrame.class",
		"viewpoint/shaders/taa_out.frag", "viewpoint/shaders/taa.frag"}
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
}

// TestMacViewpointShimBackupIsWrittenOnce: the first patch keeps the original bytes in
// Viewpoint.jar.pzpack-mac.bak, and a later patch never overwrites that backup.
func TestMacViewpointShimBackupIsWrittenOnce(t *testing.T) {
	original := vpBuildJar(t, vpFixtureEntries())
	mods := vpModsWith(t, original)
	bak := vpJarOf(mods) + ".pzpack-mac.bak"
	if _, err := applyMacViewpointPatches(mods, "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("no backup after the first patch: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("backup is not the original jar")
	}

	// Put back an unpatched jar with a different backup present: the existing backup must survive.
	if err := os.WriteFile(bak, []byte("KEEP ME"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vpJarOf(mods), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := applyMacViewpointPatches(mods, "darwin", "arm64"); err != nil {
		t.Fatal(err)
	}
	if kept, _ := os.ReadFile(bak); string(kept) != "KEEP ME" {
		t.Fatalf("existing backup was overwritten: %q", kept)
	}
}

// TestMacViewpointShimAbsentJarIsNoOp: with no Viewpoint.jar the shim returns nil and creates nothing.
func TestMacViewpointShimAbsentJarIsNoOp(t *testing.T) {
	mods := t.TempDir()
	changed, err := applyMacViewpointPatches(mods, "darwin", "arm64")
	if changed != nil || err != nil {
		t.Fatalf("absent jar = %v, %v; want nil, nil", changed, err)
	}
	if names := vpDirNames(t, mods); len(names) != 0 {
		t.Fatalf("absent jar created files: %v", names)
	}
}

// TestMacViewpointShimGate: the shim only ever changes a jar on darwin/arm64. Every other build leaves
// an unpatched jar untouched.
func TestMacViewpointShimGate(t *testing.T) {
	cases := []struct {
		goos, goarch string
		patch        bool
	}{
		{"darwin", "arm64", true},
		{"darwin", "amd64", false},
		{"windows", "amd64", false},
		{"linux", "amd64", false},
		{"linux", "arm64", false},
	}
	for _, c := range cases {
		mods := vpModsWith(t, vpBuildJar(t, vpFixtureEntries()))
		before, err := os.ReadFile(vpJarOf(mods))
		if err != nil {
			t.Fatal(err)
		}
		changed, err := applyMacViewpointPatches(mods, c.goos, c.goarch)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.goarch, err)
		}
		after, err := os.ReadFile(vpJarOf(mods))
		if err != nil {
			t.Fatal(err)
		}
		jarChanged := !bytes.Equal(before, after)
		if jarChanged != c.patch || (len(changed) > 0) != c.patch {
			t.Errorf("%s/%s: want patch=%v, got changed=%v and jar changed=%v", c.goos, c.goarch, c.patch, changed, jarChanged)
		}
	}
}

// TestApplyMacViewpointPatchesFollowsHost runs the exported entry point on this build: it patches on
// darwin/arm64 and does nothing anywhere else.
func TestApplyMacViewpointPatchesFollowsHost(t *testing.T) {
	mods := vpModsWith(t, vpBuildJar(t, vpFixtureEntries()))
	before, err := os.ReadFile(vpJarOf(mods))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ApplyMacViewpointPatches(mods)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(vpJarOf(mods))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		if len(changed) == 0 || bytes.Equal(before, after) {
			t.Fatalf("on darwin/arm64 the shim did not patch: changed=%v", changed)
		}
		return
	}
	if changed != nil || !bytes.Equal(before, after) {
		t.Fatalf("on %s/%s the shim changed the jar: changed=%v", runtime.GOOS, runtime.GOARCH, changed)
	}
}

// vpPythonWrapper runs the shim scripts against the jar named on its command line. It replaces each
// script's find_jar, the Steam search, so the real game install is never touched. It lives in its own
// temp directory, outside the scripts and the jar copies.
const vpPythonWrapper = `import importlib.util, os, sys

jar = sys.argv[1]
if "steamapps" in os.path.abspath(jar).lower():
    sys.exit("refusing: jar is inside a Steam library")
code = 0
for script in sys.argv[2:]:
    name = "vpshim_" + os.path.basename(script).replace("-", "_").replace(".py", "")
    spec = importlib.util.spec_from_file_location(name, script)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    mod.find_jar = lambda: jar
    sys.argv = [script]
    code = mod.main()
    print(os.path.basename(script) + ": exit " + str(code))
    if code != 0:
        break
sys.exit(code)
`

// vpEnvPath returns the environment override when set, or the default path.
func vpEnvPath(env, def string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return def
}

// TestMacViewpointShimMatchesPythonOnCleanJar is the equivalence check. Both Python scripts run on one
// copy of the clean Viewpoint.jar, and the Go shim runs on another copy. Every entry must match in name,
// order, DOS timestamp, external attributes, compression method (for files) and decompressed bytes, and
// both must change the same entries. It skips with a message when the clean jar, the scripts or python3
// are missing. The clean jar is only read, and it is copied before anything runs on it.
func TestMacViewpointShimMatchesPythonOnCleanJar(t *testing.T) {
	clean := vpEnvPath("PZPACK_VP_CLEAN_JAR", filepath.Join("..", "..", "..", "pz-server-modpack", "mods",
		"3809306528", "mods", "Viewpoint", "42", "media", "java", "client", "Viewpoint.jar"))
	scriptDir := vpEnvPath("PZPACK_VP_SCRIPT_DIR", filepath.Join("..", "..", ".."))
	gl40 := filepath.Join(scriptDir, "fix-viewpoint-gl40.py")
	present := filepath.Join(scriptDir, "fix-viewpoint-present.py")
	if _, err := os.Stat(clean); err != nil {
		t.Skipf("equivalence check skipped: clean Viewpoint.jar not found at %s (set PZPACK_VP_CLEAN_JAR to run it)", clean)
	}
	for _, s := range []string{gl40, present} {
		if _, err := os.Stat(s); err != nil {
			t.Skipf("equivalence check skipped: shim script not found at %s (set PZPACK_VP_SCRIPT_DIR to run it)", s)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("equivalence check skipped: python3 is not on PATH")
	}

	cleanBytes, err := os.ReadFile(clean)
	if err != nil {
		t.Fatal(err)
	}
	pyMods := vpModsWith(t, cleanBytes)
	goMods := vpModsWith(t, cleanBytes)
	wrapper := filepath.Join(t.TempDir(), "run_shims.py")
	if err := os.WriteFile(wrapper, []byte(vpPythonWrapper), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(python, "-I", "-B", wrapper, vpJarOf(pyMods), gl40, present).CombinedOutput(); err != nil {
		t.Fatalf("python shims failed: %v\n%s", err, out)
	} else {
		t.Logf("python: %s", strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", " | "))
	}
	changed, err := applyMacViewpointPatches(goMods, "darwin", "arm64")
	if err != nil {
		t.Fatalf("Go shim refused the clean jar: %v", err)
	}

	py := vpReadJar(t, vpJarOf(pyMods))
	gov := vpReadJar(t, vpJarOf(goMods))
	cl := vpReadJar(t, clean)
	if len(py) != len(gov) || len(gov) != len(cl) {
		t.Fatalf("entry counts differ: clean %d, python %d, go %d", len(cl), len(py), len(gov))
	}
	for i := range py {
		p, g := py[i], gov[i]
		switch {
		case p.name != g.name:
			t.Fatalf("entry %d: python %q, go %q (order or names differ)", i, p.name, g.name)
		case p.date != g.date || p.tm != g.tm:
			t.Fatalf("%s: DOS timestamp python %04x/%04x, go %04x/%04x", p.name, p.date, p.tm, g.date, g.tm)
		case p.attrs != g.attrs:
			t.Fatalf("%s: external attributes python %08x, go %08x", p.name, p.attrs, g.attrs)
		case !strings.HasSuffix(p.name, "/") && p.method != g.method:
			t.Fatalf("%s: compression method python %d, go %d", p.name, p.method, g.method)
		case p.sum != g.sum:
			t.Fatalf("first differing entry: %s (index %d): the bytes differ between python and go", p.name, i)
		}
	}

	var wantChanged []string
	for i := range cl {
		if cl[i].name != gov[i].name {
			t.Fatalf("entry %d: clean %q, go %q", i, cl[i].name, gov[i].name)
		}
		if cl[i].sum != py[i].sum {
			wantChanged = append(wantChanged, cl[i].name)
		}
	}
	if !reflect.DeepEqual(changed, wantChanged) {
		t.Fatalf("go changed %v, but python changed %v", changed, wantChanged)
	}

	// The second run on each copy must change nothing.
	if again, err := applyMacViewpointPatches(goMods, "darwin", "arm64"); err != nil || again != nil {
		t.Fatalf("go second run = %v, %v; want nil, nil", again, err)
	}
	bak, err := os.ReadFile(vpJarOf(goMods) + ".pzpack-mac.bak")
	if err != nil || !bytes.Equal(bak, cleanBytes) {
		t.Fatalf("backup is not the clean jar: %v", err)
	}
	t.Logf("equivalence: %d entries identical in name, order, timestamps, attributes, method and bytes; %d entries changed by both",
		len(gov), len(changed))
}

func TestViewpointShimPendingIsADryRun(t *testing.T) {
	jarBytes := vpBuildJar(t, vpFixtureEntries())
	mods := vpModsWith(t, jarBytes)
	jar := vpJarOf(mods)
	before, err := os.ReadFile(jar)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := viewpointShimPending(mods, "darwin", "arm64")
	if err != nil || !pending {
		t.Fatalf("unpatched jar: pending=%v err=%v", pending, err)
	}
	after, _ := os.ReadFile(jar)
	if !bytes.Equal(before, after) {
		t.Fatal("dry run wrote the jar")
	}
	if _, err := applyMacViewpointPatches(mods, "darwin", "arm64"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pending, err = viewpointShimPending(mods, "darwin", "arm64")
	if err != nil || pending {
		t.Fatalf("patched jar: pending=%v err=%v", pending, err)
	}
	if pending, _ := viewpointShimPending(mods, "windows", "amd64"); pending {
		t.Fatal("off darwin/arm64 nothing is pending")
	}
}
