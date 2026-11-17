package pz

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"howett.net/plist"
)

func TestHeapMBFor(t *testing.T) {
	gib := uint64(1) << 30
	cases := []struct {
		ram  uint64
		want int
	}{
		{8 * gib, 8192},
		{16 * gib, 8192},
		{16*gib + 1, 10240},
		{48 * gib, 10240},
		{0, 8192}, // unknown RAM falls back to the smaller, safe heap
	}
	for _, c := range cases {
		if got := HeapMBFor(c.ram); got != c.want {
			t.Errorf("HeapMBFor(%d)=%d want %d", c.ram, got, c.want)
		}
	}
}

func TestApplyDarwinConfig(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "Test.app")
	javaDir := filepath.Join(app, "Contents", "Java")
	if err := os.MkdirAll(javaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	original := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>Test</string>
<key>JVMOptions</key><array>
<string>-Djava.awt.headless=true</string>
<string>-Xmx3072m</string>
<string>-XX:+UseZGC</string>
</array>
</dict></plist>`
	plistPath := filepath.Join(app, "Contents", "Info.plist")
	if err := os.WriteFile(plistPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	modsDir := filepath.Join(dir, "mods")
	libs := filepath.Join(modsDir, "ZombieBuddy", "libs")
	os.MkdirAll(libs, 0o755)
	jar := []byte("fake-jar-content")
	if err := os.WriteFile(filepath.Join(libs, "ZombieBuddy.jar"), jar, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ApplyLaunchConfig(Paths{OS: "darwin", GameApp: app}, modsDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.HeapMB != HeapMBFor(totalRAMBytes()) {
		t.Fatalf("heap %d", res.HeapMB)
	}
	raw, _ := os.ReadFile(plistPath)
	var root map[string]interface{}
	if _, err := plist.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	opts := root["JVMOptions"].([]interface{})
	var heap, agent, other int
	for _, o := range opts {
		s := o.(string)
		switch {
		case s == "-Xmx10240m" || s == "-Xmx8192m":
			heap++
		case s == "-javaagent:ZombieBuddy.jar":
			agent++
		case s == "-Djava.awt.headless=true":
			other++
		}
	}
	if heap != 1 || agent != 1 || other != 1 {
		t.Fatalf("unexpected JVMOptions: %v", opts)
	}
	if b, err := os.ReadFile(filepath.Join(javaDir, "ZombieBuddy.jar")); err != nil || string(b) != string(jar) {
		t.Fatalf("jar not copied: %v", err)
	}
	if got := backupContents(t, filepath.Join(app, "Contents"), "Info.plist.pzpack"); len(got) != 1 || got[0] != original {
		t.Fatalf("expected one backup of the original plist, got %d", len(got))
	}

	res2, err := ApplyLaunchConfig(Paths{OS: "darwin", GameApp: app}, modsDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Changed) != 0 {
		t.Fatalf("second run should be a no-op, changed=%v", res2.Changed)
	}
}

func TestEditVMJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ProjectZomboid64.json")
	original := `{"mainClass":"zombie.gameStates.MainScreenState","classpath":["java/"],"vmArgs":["-Djava.awt.headless=true","-Xmx3072m"]}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	res := GameConfigResult{HeapMB: 10240}
	if err := editVMBasedJSON(path, &res, true, "-agentlib:zbNative"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	args := root["vmArgs"].([]interface{})
	var heap, agent, other int
	for _, a := range args {
		s := a.(string)
		switch s {
		case "-Xmx10240m":
			heap++
		case "-agentlib:zbNative":
			agent++
		case "-Djava.awt.headless=true":
			other++
		}
	}
	if heap != 1 || agent != 1 || other != 1 {
		t.Fatalf("unexpected vmArgs: %v", args)
	}
	if root["mainClass"] != "zombie.gameStates.MainScreenState" {
		t.Fatal("mainClass not preserved")
	}
	if got := backupContents(t, dir, "ProjectZomboid64.json.pzpack"); len(got) != 1 || got[0] != original {
		t.Fatalf("expected one backup of the original JSON, got %d", len(got))
	}
}

func TestApplyWindowsConfig(t *testing.T) {
	dir := t.TempDir()
	game := filepath.Join(dir, "ProjectZomboid")
	os.MkdirAll(game, 0o755)
	jsonPath := filepath.Join(game, "ProjectZomboid64.json")
	os.WriteFile(jsonPath, []byte(`{"mainClass":"zombie.gameStates.MainScreenState","classpath":["java/"],"vmArgs":["-Xmx3072m"]}`), 0o644)

	mods := filepath.Join(dir, "mods")
	libs := filepath.Join(mods, "ZombieBuddy", "libs")
	os.MkdirAll(libs, 0o755)
	os.WriteFile(filepath.Join(libs, "ZombieBuddy.jar"), []byte("jar"), 0o644)
	os.WriteFile(filepath.Join(libs, "zbNative.dll"), []byte("dll"), 0o644)

	res, err := ApplyLaunchConfig(Paths{OS: "windows", GameApp: game}, mods, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent == "" {
		t.Fatal("expected agent")
	}
	raw, _ := os.ReadFile(jsonPath)
	var root map[string]interface{}
	json.Unmarshal(raw, &root)
	args := root["vmArgs"].([]interface{})
	var heap, agent int
	for _, a := range args {
		switch a.(string) {
		case "-Xmx10240m":
			heap++
		case "-agentlib:zbNative":
			agent++
		}
	}
	if heap != 1 || agent != 1 {
		t.Fatalf("vmArgs %v", args)
	}
	for _, f := range []string{"ZombieBuddy.jar", "zbNative.dll"} {
		if _, err := os.Stat(filepath.Join(game, f)); err != nil {
			t.Fatalf("%s not copied", f)
		}
	}
}

func TestApplyLinuxConfig(t *testing.T) {
	dir := t.TempDir()
	game := filepath.Join(dir, "ProjectZomboid")
	os.MkdirAll(game, 0o755)
	jsonPath := filepath.Join(game, "ProjectZomboid64.json")
	os.WriteFile(jsonPath, []byte(`{"vmArgs":["-Xmx3072m"]}`), 0o644)

	mods := filepath.Join(dir, "mods")
	libs := filepath.Join(mods, "ZombieBuddy", "libs")
	os.MkdirAll(libs, 0o755)
	os.WriteFile(filepath.Join(libs, "ZombieBuddy.jar"), []byte("jar"), 0o644)

	_, err := ApplyLaunchConfig(Paths{OS: "linux", GameApp: game}, mods, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(jsonPath)
	var root map[string]interface{}
	json.Unmarshal(raw, &root)
	found := false
	for _, a := range root["vmArgs"].([]interface{}) {
		if a.(string) == "-javaagent:ZombieBuddy.jar" {
			found = true
		}
	}
	if !found {
		t.Fatalf("agent missing: %v", root["vmArgs"])
	}
	if _, err := os.Stat(filepath.Join(game, "ZombieBuddy.jar")); err != nil {
		t.Fatal("jar not copied")
	}
}

func TestBuildVMJSONEdgeCases(t *testing.T) {
	dir := t.TempDir()
	w := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	out, err := buildVMJSON(w("a.json", "\xef\xbb\xbf"+`{"vmArgs":["-Xms12288m","-XX:MaxHeapSize=2g","-javaagent:ZombieBuddy.jar=verbosity=1","-Dkeep=1"]}`), 8192, true, "-javaagent:ZombieBuddy.jar")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string][]string
	json.Unmarshal(out, &root)
	if got := strings.Join(root["vmArgs"], " "); got != "-Dkeep=1 -Xmx8192m -javaagent:ZombieBuddy.jar" {
		t.Fatalf("got %q", got)
	}
	if _, err := buildVMJSON(w("b.json", `{"vmArgs":"oops"}`), 8192, false, ""); err == nil {
		t.Fatal("non-array vmArgs must error")
	}
}

func TestDarwinMissingJarLeavesPlistUntouched(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "Project Zomboid.app")
	os.MkdirAll(filepath.Join(app, "Contents"), 0o755)
	plistPath := filepath.Join(app, "Contents", "Info.plist")
	orig := `<?xml version="1.0"?><plist version="1.0"><dict><key>JVMOptions</key><array><string>-Xmx1m</string></array></dict></plist>`
	os.WriteFile(plistPath, []byte(orig), 0o644)
	if _, err := ApplyLaunchConfig(Paths{OS: "darwin", GameApp: app}, filepath.Join(dir, "mods"), true); err == nil {
		t.Fatal("expected error for missing jar")
	}
	if b, _ := os.ReadFile(plistPath); string(b) != orig {
		t.Fatal("plist modified despite missing jar")
	}
}

// darwinAppWithOptions builds Project Zomboid.app whose Info.plist has the given
// JVMOptions, plus a ZombieBuddy.jar in the mods folder. It returns the app, the mods
// folder and the plist path.
func darwinAppWithOptions(t *testing.T, opts ...string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	app := filepath.Join(dir, "Test.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>JVMOptions</key><array>`)
	for _, o := range opts {
		b.WriteString("<string>" + o + "</string>")
	}
	b.WriteString(`</array></dict></plist>`)
	plistPath := filepath.Join(app, "Contents", "Info.plist")
	if err := os.WriteFile(plistPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	mods := filepath.Join(dir, "mods")
	libs := filepath.Join(mods, "ZombieBuddy", "libs")
	if err := os.MkdirAll(libs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libs, "ZombieBuddy.jar"), []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	return app, mods, plistPath
}

// readPlistOptions returns the JVMOptions array of a plist file.
func readPlistOptions(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if _, err := plist.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range root["JVMOptions"].([]interface{}) {
		out = append(out, o.(string))
	}
	return out
}

// linuxFixture writes a Linux install whose ProjectZomboid64.json has the given vmArgs
// array, plus a ZombieBuddy.jar in the mods folder. It returns the game dir and the mods dir.
func linuxFixture(t *testing.T, vmArgs string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	game := filepath.Join(dir, "ProjectZomboid")
	if err := os.MkdirAll(game, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(game, "ProjectZomboid64.json"), []byte(`{"vmArgs":`+vmArgs+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	mods := filepath.Join(dir, "mods")
	libs := filepath.Join(mods, "ZombieBuddy", "libs")
	if err := os.MkdirAll(libs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libs, "ZombieBuddy.jar"), []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	return game, mods
}

func TestApplyKeepsLargerUserHeapAndLogsIt(t *testing.T) {
	app, mods, plistPath := darwinAppWithOptions(t, "-Xms2g", "-Xmx12288m", "-Dkeep=1")
	p := Paths{OS: "darwin", GameApp: app, Found: true}
	res, err := ApplyLaunchConfig(p, mods, true)
	if err != nil {
		t.Fatal(err)
	}
	logged := false
	for _, c := range res.Changed {
		if strings.HasPrefix(c, "kept user -Xmx12288m") {
			logged = true
		}
	}
	if !logged {
		t.Fatalf("decision not logged in Changed: %v", res.Changed)
	}
	if got := strings.Join(readPlistOptions(t, plistPath), " "); got != "-Xmx12288m -Dkeep=1 -javaagent:ZombieBuddy.jar" {
		t.Fatalf("JVMOptions = %q", got)
	}
	res2, err := ApplyLaunchConfig(p, mods, true)
	if err != nil || len(res2.Changed) != 0 {
		t.Fatalf("second run should change nothing: changed=%v err=%v", res2.Changed, err)
	}
}

func TestApplyRefusesOnlyRealChangesWhileGameRuns(t *testing.T) {
	game, mods := linuxFixture(t, `["-Xmx3072m"]`)
	p := Paths{OS: "linux", GameApp: game, Found: true}
	jsonPath := filepath.Join(game, "ProjectZomboid64.json")
	before, _ := os.ReadFile(jsonPath)

	setGameRunning(t, true, nil)
	if _, err := ApplyLaunchConfig(p, mods, true); !errors.Is(err, ErrGameRunning) {
		t.Fatalf("change while running: err=%v, want ErrGameRunning", err)
	}
	if after, _ := os.ReadFile(jsonPath); string(after) != string(before) {
		t.Fatal("config changed while the game was running")
	}
	if _, err := os.Stat(filepath.Join(game, "ZombieBuddy.jar")); err == nil {
		t.Fatal("jar copied while the game was running")
	}

	// Once the config is current, re-applying it works while the game runs.
	setGameRunning(t, false, nil)
	if _, err := ApplyLaunchConfig(p, mods, true); err != nil {
		t.Fatal(err)
	}
	setGameRunning(t, true, nil)
	res, err := ApplyLaunchConfig(p, mods, true)
	if err != nil || len(res.Changed) != 0 {
		t.Fatalf("current config while running: changed=%v err=%v", res.Changed, err)
	}
}

func TestLaunchJSONBackupsRotate(t *testing.T) {
	game, mods := linuxFixture(t, `["-Dn=0"]`)
	p := Paths{OS: "linux", GameApp: game, Found: true}
	jsonPath := filepath.Join(game, "ProjectZomboid64.json")
	for i := 1; i <= 12; i++ {
		if err := os.WriteFile(jsonPath, []byte(fmt.Sprintf(`{"vmArgs":["-Dn=%d"]}`, i)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyLaunchConfig(p, mods, false); err != nil {
			t.Fatal(err)
		}
	}
	got := backupContents(t, game, "ProjectZomboid64.json.pzpack")
	if len(got) != backupKeep {
		t.Fatalf("kept %d backups, want %d", len(got), backupKeep)
	}
	if !strings.Contains(got[len(got)-1], `"-Dn=12"`) {
		t.Fatalf("newest backup should hold the file before the last apply:\n%s", got[len(got)-1])
	}
}

func TestNullDocumentsAreRefusedNotPanicked(t *testing.T) {
	game, mods := linuxFixture(t, `["-Xmx3072m"]`)
	if err := os.WriteFile(filepath.Join(game, "ProjectZomboid64.json"), []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLaunchConfig(Paths{OS: "linux", GameApp: game, Found: true}, mods, true); err == nil {
		t.Fatal("a JSON null document must be refused")
	}
	p := filepath.Join(t.TempDir(), "null.json")
	if err := os.WriteFile(p, []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildVMJSON(p, 8192, false, ""); err == nil {
		t.Fatal("buildVMJSON accepted a JSON null document")
	}
}

func TestCopyIfDifferentIgnoresStaleTempName(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jar")
	dst := filepath.Join(dir, "game", "ZombieBuddy.jar")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new-jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A leftover under the fixed name the old code used must not be truncated or reused.
	stale := dst + ".tmp"
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := copyIfDifferent(src, dst)
	if err != nil || !changed {
		t.Fatalf("copy: changed=%v err=%v", changed, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new-jar" {
		t.Fatalf("destination = %q", b)
	}
	if b, _ := os.ReadFile(stale); string(b) != "stale" {
		t.Fatalf("leftover temp file was touched: %q", b)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(dst); st.Mode().Perm() != 0o644 {
			t.Fatalf("copied jar mode = %v, want 0644", st.Mode().Perm())
		}
	}
	if again, err := copyIfDifferent(src, dst); err != nil || again {
		t.Fatalf("second copy should be a no-op: changed=%v err=%v", again, err)
	}
}
