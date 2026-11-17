package pz

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"howett.net/plist"
)

// GameConfigResult reports what ApplyLaunchConfig changed.
type GameConfigResult struct {
	HeapMB  int
	Agent   string
	Changed []string
}

// HeapMBFor returns the heap we set for this machine: 8 GiB up to 16 GiB of RAM,
// 10 GiB above that. Unknown RAM (0) gets the smaller 8 GiB, the safe default when
// the machine size could not be read.
func HeapMBFor(totalBytes uint64) int {
	const gib = uint64(1) << 30
	if totalBytes == 0 {
		return heapSmallMB
	}
	if totalBytes <= 16*gib {
		return heapSmallMB
	}
	return heapLargeMB
}

// configPlan is everything ApplyLaunchConfig intends to write. It is computed before
// anything touches disk, so a bad input fails with nothing half-written.
type configPlan struct {
	copies []copyStep // mod artefacts copied into the game directory; applied first
	file   fileWrite  // the launcher config rewrite; applied last
	notes  []string   // decisions, reported in Changed when the config file changes
}

// copyStep copies one mod artefact (a jar or a dll) into the game directory.
type copyStep struct {
	src, dst, label string
}

// fileWrite is the launcher config file rewrite.
type fileWrite struct {
	path, label string
	data        []byte
}

// dirty reports whether applying the plan would change any file on disk.
func (pl configPlan) dirty() (bool, error) {
	for _, c := range pl.copies {
		need, err := copyNeeded(c.src, c.dst)
		if err != nil {
			return false, err
		}
		if need {
			return true, nil
		}
	}
	cur, existed, err := readIfExists(pl.file.path)
	if err != nil {
		return false, err
	}
	return !existed || !bytes.Equal(cur, pl.file.data), nil
}

// commit applies the plan: the copies first, then the config file. A config file
// that is already current is left alone, and its notes are not logged.
func (pl configPlan) commit(res *GameConfigResult) error {
	for _, c := range pl.copies {
		changed, err := copyIfDifferent(c.src, c.dst)
		if err != nil {
			return err
		}
		if changed {
			res.Changed = append(res.Changed, c.label)
		}
	}
	changed, err := writeIfChanged(pl.file.path, pl.file.data, &res.Changed, pl.file.label)
	if err != nil {
		return err
	}
	if changed {
		res.Changed = append(res.Changed, pl.notes...)
	}
	return nil
}

// ApplyLaunchConfig makes the game load ZombieBuddy (when the Full/profile client
// layer is installed) and sets a sane -Xmx for the pack.
//
// macOS: edits Project Zomboid.app/Contents/Info.plist (JVMOptions) and copies
// ZombieBuddy.jar into Contents/Java/.
// Windows: edits ProjectZomboid64.json (vmArgs) and copies ZombieBuddy.jar +
// zbNative.dll into the game directory, using -agentlib:zbNative.
// Linux: edits ProjectZomboid64.json and copies ZombieBuddy.jar next to it.
//
// Everything is planned before anything is written. If the plan would change a file
// while the game is running, nothing is written and ErrGameRunning is returned. A
// plan that changes nothing succeeds even while the game runs, so re-applying a
// current config still works.
func ApplyLaunchConfig(p Paths, modsDir string, withClientLayer bool) (GameConfigResult, error) {
	res := GameConfigResult{HeapMB: HeapMBFor(totalRAMBytes())}
	if err := p.RequireGame(); err != nil {
		return res, err
	}
	if withClientLayer {
		res.Agent = "ZombieBuddy"
	}
	var (
		plan configPlan
		err  error
	)
	switch p.OS {
	case "darwin":
		plan, err = planDarwin(p, modsDir, withClientLayer, &res)
	case "windows":
		plan, err = planWindows(p, modsDir, withClientLayer, &res)
	default:
		plan, err = planLinux(p, modsDir, withClientLayer, &res)
	}
	if err != nil {
		return res, err
	}
	dirty, err := plan.dirty()
	if err != nil {
		return res, err
	}
	if p.OS == "darwin" && withClientLayer {
		// The Viewpoint jar is written by the shim after commit, so it counts as a write here.
		pending, err := ViewpointShimPending(modsDir)
		if err != nil {
			return res, err
		}
		dirty = dirty || pending
	}
	if err := guardGameWrite(dirty); err != nil {
		return res, err
	}
	if err := plan.commit(&res); err != nil {
		return res, err
	}
	if p.OS == "darwin" && withClientLayer {
		// The Mac Viewpoint shim runs after the ZombieBuddy jar copy. It is a no-op unless the
		// build is darwin/arm64, and its refusals are returned as the launch error.
		changed, err := ApplyMacViewpointPatches(modsDir)
		for _, name := range changed {
			res.Changed = append(res.Changed, "Viewpoint.jar "+name)
		}
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// isHeapArg reports JVM args that fix heap sizes; we own these (a stale -Xms above
// our -Xmx makes the JVM refuse to start).
func isHeapArg(a string) bool {
	for _, pre := range []string{"-Xmx", "-Xms", "-XX:InitialHeapSize", "-XX:MaxHeapSize", "-XX:MaxRAM"} {
		if strings.HasPrefix(a, pre) {
			return true
		}
	}
	return false
}

// isAgentArg matches any spelling of our agent (with =options, any case).
func isAgentArg(a, agent string) bool {
	la, lg := strings.ToLower(a), strings.ToLower(agent)
	return la == lg || strings.HasPrefix(la, lg+"=")
}

func zombieBuddyJar(modsDir string) string {
	return filepath.Join(modsDir, "ZombieBuddy", "libs", "ZombieBuddy.jar")
}

// planDarwin plans the macOS rewrite of Info.plist JVMOptions and the jar copy. A
// missing ZombieBuddy.jar is refused before the plist is read.
func planDarwin(p Paths, modsDir string, withClient bool, res *GameConfigResult) (configPlan, error) {
	plistPath := filepath.Join(p.GameApp, "Contents", "Info.plist")
	jar := zombieBuddyJar(modsDir)
	if withClient {
		if _, err := os.Stat(jar); err != nil {
			return configPlan{}, fmt.Errorf("ZombieBuddy.jar missing (%s): not touching Info.plist: %w", jar, err)
		}
	}
	raw, err := os.ReadFile(plistPath)
	if err != nil {
		return configPlan{}, fmt.Errorf("read %s: %w", plistPath, err)
	}
	var root map[string]interface{}
	if _, err := plist.Unmarshal(raw, &root); err != nil {
		return configPlan{}, fmt.Errorf("parse %s: %w", plistPath, err)
	}
	if root == nil {
		return configPlan{}, fmt.Errorf("parse %s: top level is not a dict", plistPath)
	}
	opts, _ := root["JVMOptions"].([]interface{})
	ownAgent := func(s string) bool {
		ls := strings.ToLower(s)
		return strings.Contains(ls, "zombiebuddy.jar") && strings.HasPrefix(ls, "-javaagent:")
	}
	newOpts, note := rewriteVMArgs(opts, res.HeapMB, ownAgent)
	if withClient {
		newOpts = append(newOpts, "-javaagent:ZombieBuddy.jar")
	}
	root["JVMOptions"] = newOpts
	out, err := plist.Marshal(root, plist.XMLFormat)
	if err != nil {
		return configPlan{}, fmt.Errorf("encode plist: %w", err)
	}
	pl := configPlan{file: fileWrite{path: plistPath, label: "Info.plist JVMOptions", data: out}}
	if note != "" {
		pl.notes = append(pl.notes, note)
	}
	if withClient {
		pl.copies = []copyStep{{
			src:   jar,
			dst:   filepath.Join(p.GameApp, "Contents", "Java", "ZombieBuddy.jar"),
			label: "Contents/Java/ZombieBuddy.jar",
		}}
	}
	return pl, nil
}

// planWindows plans the Windows rewrite of ProjectZomboid64.json vmArgs and the jar
// and dll copies into the game directory.
func planWindows(p Paths, modsDir string, withClient bool, res *GameConfigResult) (configPlan, error) {
	jsonPath := filepath.Join(p.GameApp, "ProjectZomboid64.json")
	newJSON, note, err := buildVMJSONNote(jsonPath, res.HeapMB, withClient, "-agentlib:zbNative")
	if err != nil {
		return configPlan{}, err
	}
	pl := configPlan{file: fileWrite{path: jsonPath, label: filepath.Base(jsonPath) + " vmArgs", data: newJSON}}
	if note != "" {
		pl.notes = append(pl.notes, note)
	}
	if withClient {
		jar := zombieBuddyJar(modsDir)
		dll := filepath.Join(modsDir, "ZombieBuddy", "libs", "zbNative.dll")
		for _, f := range []string{jar, dll} {
			if _, err := os.Stat(f); err != nil {
				return configPlan{}, fmt.Errorf("missing %s: %w", f, err)
			}
		}
		pl.copies = []copyStep{
			{src: jar, dst: filepath.Join(p.GameApp, "ZombieBuddy.jar"), label: "ZombieBuddy.jar"},
			{src: dll, dst: filepath.Join(p.GameApp, "zbNative.dll"), label: "zbNative.dll"},
		}
	}
	return pl, nil
}

// planLinux plans the Linux rewrite of ProjectZomboid64.json vmArgs and the jar copy
// next to it. The JSON may sit in the install root or in a projectzomboid/ subfolder.
func planLinux(p Paths, modsDir string, withClient bool, res *GameConfigResult) (configPlan, error) {
	jsonPath := filepath.Join(p.GameApp, "ProjectZomboid64.json")
	gameDir := p.GameApp
	if _, err := os.Stat(jsonPath); err != nil {
		altDir := filepath.Join(p.GameApp, "projectzomboid")
		alt := filepath.Join(altDir, "ProjectZomboid64.json")
		if _, err2 := os.Stat(alt); err2 == nil {
			jsonPath, gameDir = alt, altDir
		}
	}
	newJSON, note, err := buildVMJSONNote(jsonPath, res.HeapMB, withClient, "-javaagent:ZombieBuddy.jar")
	if err != nil {
		return configPlan{}, err
	}
	pl := configPlan{file: fileWrite{path: jsonPath, label: filepath.Base(jsonPath) + " vmArgs", data: newJSON}}
	if note != "" {
		pl.notes = append(pl.notes, note)
	}
	if withClient {
		pl.copies = []copyStep{{
			src:   zombieBuddyJar(modsDir),
			dst:   filepath.Join(gameDir, "ZombieBuddy.jar"),
			label: "ZombieBuddy.jar",
		}}
	}
	return pl, nil
}

// buildVMJSON computes the rewritten launcher JSON without touching disk, so a bad
// file is rejected before anything else (jar/dll copies) is written.
func buildVMJSON(path string, heapMB int, withClient bool, agent string) ([]byte, error) {
	out, _, err := buildVMJSONNote(path, heapMB, withClient, agent)
	return out, err
}

// buildVMJSONNote is buildVMJSON that also returns the decision note ("" when the
// computed -Xmx was written).
func buildVMJSONNote(path string, heapMB int, withClient bool, agent string) ([]byte, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	if root == nil {
		return nil, "", fmt.Errorf("parse %s: top level is not a JSON object", path)
	}
	var args []interface{}
	if v, present := root["vmArgs"]; present {
		var ok bool
		if args, ok = v.([]interface{}); !ok {
			return nil, "", fmt.Errorf("%s: vmArgs is not an array; refusing to overwrite", path)
		}
	}
	ownAgent := func(s string) bool { return agent != "" && isAgentArg(s, agent) }
	newArgs, note := rewriteVMArgs(args, heapMB, ownAgent)
	if withClient {
		newArgs = append(newArgs, agent)
	}
	root["vmArgs"] = newArgs
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), note, nil
}

// writeIfChanged replaces path with want when the content differs. The previous file
// is first kept as a rotating backup. It reports whether it wrote the file.
func writeIfChanged(path string, want []byte, changed *[]string, label string) (bool, error) {
	cur, existed, err := readIfExists(path)
	if err != nil {
		return false, err
	}
	if existed && bytes.Equal(cur, want) {
		return false, nil
	}
	if existed {
		if _, err := writeBackup(filepath.Dir(path), filepath.Base(path)+".pzpack", cur); err != nil {
			return false, err
		}
	}
	if err := writeFileAtomic(path, want); err != nil {
		return false, err
	}
	*changed = append(*changed, label)
	return true, nil
}

// copyNeeded reports whether dst is missing or differs from src. Sizes are compared
// first, so the hashes are only read when the sizes match.
func copyNeeded(src, dst string) (bool, error) {
	s, err := os.Stat(src)
	if err != nil {
		return false, fmt.Errorf("missing %s: %w", src, err)
	}
	d, err := os.Stat(dst)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if d.Size() != s.Size() {
		return true, nil
	}
	same, err := sameSHA(src, dst)
	if err != nil {
		return false, err
	}
	return !same, nil
}

// copyIfDifferent copies src over dst when they differ. The copy goes through a
// uniquely named temp file in dst's directory and then a replace with bounded retry,
// so a leftover temp file is never truncated and a transient sharing violation does
// not fail the copy outright.
func copyIfDifferent(src, dst string) (bool, error) {
	need, err := copyNeeded(src, dst)
	if err != nil || !need {
		return false, err
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(name)
		return false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return false, err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return false, err
	}
	if err := replaceFile(name, dst); err != nil {
		os.Remove(name)
		return false, err
	}
	return true, nil
}

func sameSHA(a, b string) (bool, error) {
	ha, err := fileSHA(a)
	if err != nil {
		return false, err
	}
	hb, err := fileSHA(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// editVMBasedJSON rewrites one launcher JSON in place.
func editVMBasedJSON(path string, res *GameConfigResult, withClient bool, agent string) error {
	out, err := buildVMJSON(path, res.HeapMB, withClient, agent)
	if err != nil {
		return err
	}
	_, err = writeIfChanged(path, out, &res.Changed, filepath.Base(path)+" vmArgs")
	return err
}
