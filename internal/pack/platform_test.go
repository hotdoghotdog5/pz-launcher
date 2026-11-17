package pack

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// platformPackDoc builds a pack.json with one profile, "full", over the base and client layers.
// items are complete item objects, as built by platformItemJSON.
func platformPackDoc(loadOrder []string, items ...string) string {
	q := make([]string, len(loadOrder))
	for i, w := range loadOrder {
		q[i] = fmt.Sprintf("%q", w)
	}
	return fmt.Sprintf(`{"schema":1,"name":"t","version":"v","profiles":{"full":{"label":"Full","layers":["base","client"],"approx_bytes":1,"stub_layer":""}},"load_order":[%s],"items":[%s]}`,
		strings.Join(q, ","), strings.Join(items, ","))
}

// platformItemJSON builds one item with a single mod folder and one mod id. extra is more object
// members, such as `"platforms":["darwin/arm64"]`, or empty.
func platformItemJSON(wid, layer, modID, extra string) string {
	members := fmt.Sprintf(`"workshop_id":%q,"title":%q,"layer":%q,"folders":[{"name":%q,"src":%q}],"bytes":1,"mod_ids":[%q]`,
		wid, "t-"+wid, layer, "F"+wid, "mods/"+wid+"/F"+wid, modID)
	if extra != "" {
		members += "," + extra
	}
	return "{" + members + "}"
}

// writePackJSON writes doc to a temp file and returns its path.
func writePackJSON(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pack.json")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// gatedPack is a 42.22-style manifest: a base item every build installs, a Mac-only client item,
// and a client item for every build.
func gatedPack(t *testing.T) *Pack {
	t.Helper()
	doc := platformPackDoc([]string{"100", "3823866661", "200"},
		platformItemJSON("100", "base", "BaseMod", ""),
		platformItemJSON("3823866661", "client", "ViewpointMetal", `"platforms":["darwin/arm64"]`),
		platformItemJSON("200", "client", "Viewpoint", ""),
	)
	p, err := Load(writePackJSON(t, doc))
	if err != nil {
		t.Fatalf("Load rejected a valid gated manifest: %v", err)
	}
	return p
}

// workshopIDs lists the workshop ids of items in order.
func workshopIDs(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.WorkshopID)
	}
	return out
}

// TestPlatformMatches covers the pure matcher on the builds the launcher cares about: an item with
// no platforms, and gates on darwin/arm64, darwin/amd64, windows/amd64 and linux/amd64.
func TestPlatformMatches(t *testing.T) {
	cases := []struct {
		name      string
		platforms []string
		goos      string
		goarch    string
		want      bool
	}{
		{"no platforms on darwin/arm64", nil, "darwin", "arm64", true},
		{"no platforms on darwin/amd64", nil, "darwin", "amd64", true},
		{"no platforms on windows/amd64", nil, "windows", "amd64", true},
		{"no platforms on linux/amd64", nil, "linux", "amd64", true},
		{"darwin/arm64 gate on darwin/arm64", []string{"darwin/arm64"}, "darwin", "arm64", true},
		{"darwin/arm64 gate on darwin/amd64", []string{"darwin/arm64"}, "darwin", "amd64", false},
		{"darwin/arm64 gate on windows/amd64", []string{"darwin/arm64"}, "windows", "amd64", false},
		{"darwin/arm64 gate on linux/amd64", []string{"darwin/arm64"}, "linux", "amd64", false},
		{"bare darwin on darwin/amd64", []string{"darwin"}, "darwin", "amd64", true},
		{"bare darwin on darwin/arm64", []string{"darwin"}, "darwin", "arm64", true},
		{"bare darwin on linux/amd64", []string{"darwin"}, "linux", "amd64", false},
		{"any of several entries", []string{"windows/amd64", "linux"}, "linux", "arm64", true},
		{"none of several entries", []string{"windows/amd64", "linux"}, "darwin", "arm64", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := platformMatches(c.platforms, c.goos, c.goarch); got != c.want {
				t.Fatalf("platformMatches(%v, %s, %s) = %v, want %v", c.platforms, c.goos, c.goarch, got, c.want)
			}
		})
	}
}

// TestCheckPlatformAcceptsKnownEntries: a bare GOOS, or a GOOS/GOARCH pair the toolchain builds.
func TestCheckPlatformAcceptsKnownEntries(t *testing.T) {
	for _, ok := range []string{"darwin/arm64", "darwin/amd64", "darwin", "linux/amd64", "linux/riscv64", "windows/amd64", "windows"} {
		if err := checkPlatform(ok); err != nil {
			t.Errorf("checkPlatform(%q) refused a known platform: %v", ok, err)
		}
	}
}

// TestCheckPlatformRefusesUnknownEntries: unknown OS names, pairs the toolchain does not build,
// wrong case, stray whitespace, and malformed separators.
func TestCheckPlatformRefusesUnknownEntries(t *testing.T) {
	for _, bad := range []string{
		"", "macos", "Darwin/arm64", "darwin/ARM64", "darwin/arm", "windows/arm",
		"plan9/arm64", "darwin/", "/arm64", "darwin/arm64/extra", "darwin ", " darwin",
	} {
		if err := checkPlatform(bad); err == nil {
			t.Errorf("checkPlatform(%q) accepted an unknown platform", bad)
		}
	}
}

// TestLoadRefusesUnknownPlatform: Load-time validation rejects an unknown platform string, so a
// typo in pack.json fails loudly instead of silently gating the item off everywhere.
func TestLoadRefusesUnknownPlatform(t *testing.T) {
	for _, bad := range []string{"macos", "darwin/arm"} {
		doc := platformPackDoc([]string{"100"},
			platformItemJSON("100", "client", "M", fmt.Sprintf(`"platforms":[%q]`, bad)))
		if _, err := Load(writePackJSON(t, doc)); err == nil {
			t.Errorf("Load accepted platform %q", bad)
		}
	}
}

// TestLoadAcceptsPlatforms: a valid platforms list loads and is kept on the item.
func TestLoadAcceptsPlatforms(t *testing.T) {
	doc := platformPackDoc([]string{"100"},
		platformItemJSON("100", "client", "M", `"platforms":["darwin/arm64","linux"]`))
	p, err := Load(writePackJSON(t, doc))
	if err != nil {
		t.Fatalf("Load rejected a valid platforms list: %v", err)
	}
	want := []string{"darwin/arm64", "linux"}
	if !reflect.DeepEqual(p.Items[0].Platforms, want) {
		t.Fatalf("Platforms = %v, want %v", p.Items[0].Platforms, want)
	}
}

// TestSelectedAppliesPlatformGate: the gated item is selected only on its own build, and the
// ungated items are selected everywhere.
func TestSelectedAppliesPlatformGate(t *testing.T) {
	p := gatedPack(t)
	cases := []struct {
		goos, goarch string
		want         []string
	}{
		{"darwin", "arm64", []string{"100", "3823866661", "200"}},
		{"darwin", "amd64", []string{"100", "200"}},
		{"windows", "amd64", []string{"100", "200"}},
		{"linux", "amd64", []string{"100", "200"}},
	}
	for _, c := range cases {
		items, err := p.selectedFor("full", c.goos, c.goarch)
		if err != nil {
			t.Fatalf("selectedFor(%s/%s): %v", c.goos, c.goarch, err)
		}
		if got := workshopIDs(items); !reflect.DeepEqual(got, c.want) {
			t.Errorf("selectedFor(%s/%s) = %v, want %v", c.goos, c.goarch, got, c.want)
		}
	}
}

// TestBuildPlanSkipsGatedItemOnLinux: the install plan drops the Mac-only item on a Linux build,
// so its folders and bytes are not counted or installed.
func TestBuildPlanSkipsGatedItemOnLinux(t *testing.T) {
	p := gatedPack(t)
	plan, err := buildPlanFor(p, "full", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got := workshopIDs(plan.Items); !reflect.DeepEqual(got, []string{"100", "200"}) {
		t.Fatalf("linux plan items = %v, want [100 200]", got)
	}
	if plan.Folders != 2 || plan.Bytes != 2 {
		t.Fatalf("linux plan folders=%d bytes=%d, want 2 and 2", plan.Folders, plan.Bytes)
	}
	if _, ok := plan.ModIDs()["ViewpointMetal"]; ok {
		t.Fatal("linux plan lists the darwin/arm64 mod id")
	}

	mac, err := buildPlanFor(p, "full", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got := workshopIDs(mac.Items); !reflect.DeepEqual(got, []string{"100", "3823866661", "200"}) {
		t.Fatalf("darwin/arm64 plan items = %v", got)
	}
	if mac.Folders != 3 || mac.Bytes != 3 {
		t.Fatalf("darwin/arm64 plan folders=%d bytes=%d, want 3 and 3", mac.Folders, mac.Bytes)
	}
}

// TestModIDsForExcludesDarwinItemOnLinuxServer is the --server-ini case: a Linux server's mod list
// must not name the darwin/arm64-only mod, while the Mac build's list must.
func TestModIDsForExcludesDarwinItemOnLinuxServer(t *testing.T) {
	p := gatedPack(t)
	linux, err := p.modIDsFor("full", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"BaseMod", "Viewpoint"}; !reflect.DeepEqual(linux, want) {
		t.Fatalf("linux ModIDsFor = %v, want %v", linux, want)
	}
	mac, err := p.modIDsFor("full", "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"BaseMod", "ViewpointMetal", "Viewpoint"}; !reflect.DeepEqual(mac, want) {
		t.Fatalf("darwin/arm64 ModIDsFor = %v, want %v", mac, want)
	}
}

// TestExportedWrappersFollowRunningBuild: Selected, BuildPlan and ModIDsFor keep their signatures
// and answer for the build the test runs on.
func TestExportedWrappersFollowRunningBuild(t *testing.T) {
	p := gatedPack(t)
	wantItems, err := p.selectedFor("full", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	gotItems, err := p.Selected("full")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(workshopIDs(gotItems), workshopIDs(wantItems)) {
		t.Fatalf("Selected = %v, want %v on %s/%s", workshopIDs(gotItems), workshopIDs(wantItems), runtime.GOOS, runtime.GOARCH)
	}
	plan, err := BuildPlan(p, "full")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(workshopIDs(plan.Items), workshopIDs(wantItems)) {
		t.Fatalf("BuildPlan items = %v, want %v", workshopIDs(plan.Items), workshopIDs(wantItems))
	}
	ids, err := p.ModIDsFor("full")
	if err != nil {
		t.Fatal(err)
	}
	wantIDs, err := p.modIDsFor("full", runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("ModIDsFor = %v, want %v", ids, wantIDs)
	}
}
