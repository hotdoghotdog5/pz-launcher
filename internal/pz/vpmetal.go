package pz

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// The Mac Viewpoint shim is a Go port of fix-viewpoint-gl40.py and fix-viewpoint-present.py in
// default-workspace. Both scripts rewrite Viewpoint.jar in place for Apple silicon:
//
//   - Gl.class: the GL40.glPatchParameteri call becomes an equal-length no-op. LWJGL aborts the JVM
//     on that call, because GL 4.0 has no entry point in the macOS context.
//   - ImGuiFrame.class: the "#version 130" GLSL header becomes "#version 330", which Apple's GLSL
//     compiler accepts.
//   - taa_out.frag and taa.frag: texelFetch becomes a texture() fetch, so the present pass does not
//     render black under the GLSL-to-Metal translation.
//   - Two shaderpack pack.properties files: the default antialiasing changes from taaPlus to fxaa, so
//     Viewpoint does not route presentation back through taa.frag.
//
// The byte patterns, count checks, refusals and text rewrites are the scripts'. The jar is rewritten
// once, through a temp file and a rename. Every entry keeps its name, order, DOS timestamp, external
// attributes and compression method. Unchanged entries keep their content.

const (
	vpJarRel       = "Viewpoint/42/media/java/client/Viewpoint.jar"
	vpBackupSuffix = ".pzpack-mac.bak"

	vpGLClass    = "viewpoint/platform/Gl.class"
	vpImGuiClass = "viewpoint/platform/ImGuiFrame.class"
	vpSentinel   = "viewpoint/shaders/taa_out.frag"

	vpVer130 = "#version 130"
	vpVer330 = "#version 330"

	vpTexelFetch = "texelFetch"
	vpTaaPlus    = "option.antialiasing.default=taaPlus"
	vpFxaa       = "option.antialiasing.default=fxaa"
)

var (
	// vpGLCall is invokestatic #16 (GL40.glPatchParameteri(II)V) as b8 00 10.
	vpGLCall = []byte{0xb8, 0x00, 0x10}
	// vpGLSkip is pop; pop; nop (57 57 00): the same length and the same stack effect.
	vpGLSkip = []byte{0x57, 0x57, 0x00}
)

// vpFetchHelper replaces "void main() {" in taa.frag. It defines texelFetch through the sampler.
const vpFetchHelper = `vec4 vpFetch(sampler2D t, ivec2 c) {
  ivec2 sz = textureSize(t, 0);
  return texture(t, (vec2(clamp(c, ivec2(0), sz - 1)) + 0.5) / vec2(sz));
}
void main() {`

// vpTaaOutOld and vpTaaOutNew rewrite the RCAS present in taa_out.frag, which runs when TAA is off.
const vpTaaOutOld = `    ivec2 last = textureSize(uColor, 0) - 1, p = clamp(ivec2(vUv * vec2(last + 1)), ivec2(0), last);
    c = rcas(texelFetch(uColor, p, 0).rgb, texelFetch(uColor, clamp(p + ivec2(0, 1), ivec2(0), last), 0).rgb,
             texelFetch(uColor, clamp(p - ivec2(1, 0), ivec2(0), last), 0).rgb,
             texelFetch(uColor, clamp(p + ivec2(1, 0), ivec2(0), last), 0).rgb,
             texelFetch(uColor, clamp(p - ivec2(0, 1), ivec2(0), last), 0).rgb);`

const vpTaaOutNew = `    vec2 texel = 1.0 / vec2(textureSize(uColor, 0));
    c = rcas(texture(uColor, vUv).rgb, texture(uColor, vUv + vec2(0.0, texel.y)).rgb,
             texture(uColor, vUv - vec2(texel.x, 0.0)).rgb, texture(uColor, vUv + vec2(texel.x, 0.0)).rgb,
             texture(uColor, vUv - vec2(0.0, texel.y)).rgb);`

// vpTextEdit is one exact replacement. Every occurrence is replaced, as str.replace does in Python.
type vpTextEdit struct{ old, new string }

// vpTextTarget is one jar entry the present shim rewrites. unpatched is a substring whose presence
// means the entry still needs the rewrite. An entry the jar does not contain is skipped.
type vpTextTarget struct {
	name      string
	unpatched string
	edits     []vpTextEdit
}

// vpPresentTargets lists the present-pass rewrites in the script's order.
var vpPresentTargets = []vpTextTarget{
	{
		name:      "viewpoint/shaders/taa_out.frag",
		unpatched: vpTexelFetch,
		edits:     []vpTextEdit{{vpTaaOutOld, vpTaaOutNew}},
	},
	{
		name:      "viewpoint/shaderpacks/normal/pack.properties",
		unpatched: vpTaaPlus,
		edits:     []vpTextEdit{{vpTaaPlus, vpFxaa}},
	},
	{
		name:      "viewpoint/shaderpacks/library/pack.properties",
		unpatched: vpTaaPlus,
		edits:     []vpTextEdit{{vpTaaPlus, vpFxaa}},
	},
	{
		name:      "viewpoint/shaders/taa.frag",
		unpatched: vpTexelFetch,
		edits: []vpTextEdit{
			{"void main() {", vpFetchHelper},
			{"texelFetch(uCurrent, clamp(at + OFFSETS[i], ivec2(0), size - 1), 0)", "vpFetch(uCurrent, at + OFFSETS[i])"},
			{"texelFetch(uHistory, h + OFFSETS[i], 0)", "vpFetch(uHistory, h + OFFSETS[i])"},
			{"texelFetch(uHistoryCount, h, 0)", "vpFetch(uHistoryCount, h)"},
		},
	},
}

// vpPlan is what the shim would change in one jar. edits holds the replacement bytes by entry name,
// and changed lists those entries in jar order.
type vpPlan struct {
	edits   map[string][]byte
	changed []string
}

// planViewpointShim applies the scripts' status and refusal rules to a jar. It never writes.
func planViewpointShim(zr *zip.Reader) (vpPlan, error) {
	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		if _, dup := byName[f.Name]; dup {
			return vpPlan{}, fmt.Errorf("refusing: entry %q appears twice", f.Name)
		}
		byName[f.Name] = f
	}
	for _, need := range []string{vpGLClass, vpImGuiClass, vpSentinel} {
		if byName[need] == nil {
			return vpPlan{}, fmt.Errorf("refusing: %s is missing, so this is not a Viewpoint.jar the shim recognises", need)
		}
	}
	edits := map[string][]byte{}

	// fix-viewpoint-gl40.py: the glPatchParameteri call, then the ImGui shader version.
	gl, err := vpReadEntry(byName[vpGLClass])
	if err != nil {
		return vpPlan{}, err
	}
	glSkipped := bytes.Count(gl, vpGLCall) == 0 && bytes.Count(gl, vpGLSkip) >= 1
	if !glSkipped {
		if n := bytes.Count(gl, vpGLCall); n != 1 {
			return vpPlan{}, fmt.Errorf("refusing: expected exactly one glPatchParameteri call in %s, found %d", vpGLClass, n)
		}
		edits[vpGLClass] = bytes.ReplaceAll(gl, vpGLCall, vpGLSkip)
	}
	im, err := vpReadEntry(byName[vpImGuiClass])
	if err != nil {
		return vpPlan{}, err
	}
	v130, v330 := []byte(vpVer130), []byte(vpVer330)
	imguiPatched := !bytes.Contains(im, v130) && bytes.Contains(im, v330)
	if !imguiPatched {
		if n := bytes.Count(im, v130); n != 1 {
			return vpPlan{}, fmt.Errorf("refusing: expected exactly one '#version 130' in %s, found %d", vpImGuiClass, n)
		}
		edits[vpImGuiClass] = bytes.ReplaceAll(im, v130, v330)
	}

	// fix-viewpoint-present.py: the present shaders and the two antialiasing defaults.
	for _, t := range vpPresentTargets {
		f := byName[t.name]
		if f == nil {
			continue
		}
		raw, err := vpReadEntry(f)
		if err != nil {
			return vpPlan{}, err
		}
		if !utf8.Valid(raw) {
			return vpPlan{}, fmt.Errorf("refusing: %s is not UTF-8", t.name)
		}
		txt := string(raw)
		if !strings.Contains(txt, t.unpatched) {
			continue
		}
		rewritten, err := vpRewriteText(t, txt)
		if err != nil {
			return vpPlan{}, err
		}
		edits[t.name] = []byte(rewritten)
	}

	plan := vpPlan{edits: edits}
	for _, f := range zr.File {
		if _, ok := edits[f.Name]; ok {
			plan.changed = append(plan.changed, f.Name)
		}
	}
	return plan, nil
}

// vpRewriteText applies one target's edits in order. Each anchor must be present when its edit runs,
// and no texelFetch may remain afterwards.
func vpRewriteText(t vpTextTarget, txt string) (string, error) {
	for _, e := range t.edits {
		if !strings.Contains(txt, e.old) {
			return "", fmt.Errorf("refusing: anchor not found in %s; the mod changed this shader, so re-derive the rewrite rather than guessing", t.name)
		}
		txt = strings.ReplaceAll(txt, e.old, e.new)
	}
	if strings.Contains(txt, vpTexelFetch) {
		return "", fmt.Errorf("refusing: texelFetch still present in %s after rewrite", t.name)
	}
	return txt, nil
}

// rewriteViewpointJar writes every entry in the original order. Entries named in edits get their new
// bytes. The returned sums hold the SHA-256 each entry must have after the rewrite.
func rewriteViewpointJar(zr *zip.Reader, edits map[string][]byte) ([]byte, map[string][sha256.Size]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	sums := make(map[string][sha256.Size]byte, len(zr.File))
	for _, f := range zr.File {
		data, err := vpReadEntry(f)
		if err != nil {
			return nil, nil, err
		}
		if repl, ok := edits[f.Name]; ok {
			data = repl
		}
		sums[f.Name] = sha256.Sum256(data)
		w, err := zw.CreateHeader(&zip.FileHeader{
			Name:          f.Name,
			Method:        f.Method,
			ModifiedDate:  f.ModifiedDate,
			ModifiedTime:  f.ModifiedTime,
			ExternalAttrs: f.ExternalAttrs,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("write %s: %w", f.Name, err)
		}
		if _, err := w.Write(data); err != nil {
			return nil, nil, fmt.Errorf("write %s: %w", f.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), sums, nil
}

// verifyViewpointRewrite parses the bytes about to be installed. It refuses unless they hold the same
// entries in the same order, with the same metadata, the planned contents, and no shim work left. It
// runs before anything is written, so a bad rewrite never replaces the jar.
func verifyViewpointRewrite(orig *zip.Reader, out []byte, sums map[string][sha256.Size]byte) error {
	zr, err := zip.NewReader(bytes.NewReader(out), int64(len(out)))
	if err != nil {
		return fmt.Errorf("verify: rewritten jar does not parse: %w", err)
	}
	if len(zr.File) != len(orig.File) {
		return fmt.Errorf("verify: rewritten jar has %d entries, want %d", len(zr.File), len(orig.File))
	}
	for i, f := range zr.File {
		o := orig.File[i]
		if f.Name != o.Name || f.ModifiedDate != o.ModifiedDate || f.ModifiedTime != o.ModifiedTime || f.ExternalAttrs != o.ExternalAttrs {
			return fmt.Errorf("verify: entry %d (%s) changed its name, timestamp or attributes", i, o.Name)
		}
		if f.Method != o.Method && !strings.HasSuffix(o.Name, "/") {
			return fmt.Errorf("verify: %s changed its compression method", o.Name)
		}
		data, err := vpReadEntry(f)
		if err != nil {
			return fmt.Errorf("verify: %w", err)
		}
		if sha256.Sum256(data) != sums[f.Name] {
			return fmt.Errorf("verify: %s does not hold the planned contents", f.Name)
		}
	}
	again, err := planViewpointShim(zr)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	if len(again.changed) != 0 {
		return fmt.Errorf("verify: %d entries still need the shim after the rewrite", len(again.changed))
	}
	return nil
}

// backupViewpointJarOnce keeps the jar as it was before the first patch. It writes the backup only
// when none exists, so a later patch never overwrites the original.
func backupViewpointJarOnce(jar string, raw []byte) error {
	bak := jar + vpBackupSuffix
	_, err := os.Stat(bak)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("check backup %s: %w", bak, err)
	}
	if err := writeFileAtomic(bak, raw); err != nil {
		return fmt.Errorf("write backup %s: %w", bak, err)
	}
	return nil
}

// vpReadEntry returns one entry's bytes. The zip reader checks the CRC at the end of the stream, so a
// corrupt entry fails here instead of being patched or copied.
func vpReadEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.Name, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	return data, nil
}

// applyMacViewpointPatches is ApplyMacViewpointPatches for an explicit build. The shim is for the
// arm64 Metal translation, so every other build returns at once.
func applyMacViewpointPatches(modsDir, goos, goarch string) ([]string, error) {
	if goos != "darwin" || goarch != "arm64" {
		return nil, nil
	}
	jar := filepath.Join(modsDir, filepath.FromSlash(vpJarRel))
	raw, err := os.ReadFile(jar)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", jar, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("refusing: %s is not a zip archive: %w", jar, err)
	}
	plan, err := planViewpointShim(zr)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", jar, err)
	}
	if len(plan.changed) == 0 {
		return nil, nil
	}
	out, sums, err := rewriteViewpointJar(zr, plan.edits)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", jar, err)
	}
	if err := verifyViewpointRewrite(zr, out, sums); err != nil {
		return nil, fmt.Errorf("%s: %w", jar, err)
	}
	if err := backupViewpointJarOnce(jar, raw); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(jar, out); err != nil {
		return nil, fmt.Errorf("write %s: %w", jar, err)
	}
	return plan.changed, nil
}

// ViewpointShimPending reports whether the Mac shim would change the Viewpoint jar on this build. It
// reads the jar and plans the edits, and writes nothing. It is false off darwin/arm64, when the jar is
// absent, and when the jar is already patched. It is the dry-run the running-game guard consults.
func ViewpointShimPending(modsDir string) (bool, error) {
	return viewpointShimPending(modsDir, runtime.GOOS, runtime.GOARCH)
}

func viewpointShimPending(modsDir, goos, goarch string) (bool, error) {
	if goos != "darwin" || goarch != "arm64" {
		return false, nil
	}
	jar := filepath.Join(modsDir, filepath.FromSlash(vpJarRel))
	raw, err := os.ReadFile(jar)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", jar, err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return false, fmt.Errorf("refusing: %s is not a zip archive: %w", jar, err)
	}
	plan, err := planViewpointShim(zr)
	if err != nil {
		return false, fmt.Errorf("%s: %w", jar, err)
	}
	return len(plan.changed) > 0, nil
}

// ApplyMacViewpointPatches applies the Mac arm64 Viewpoint shim to
// modsDir/Viewpoint/42/media/java/client/Viewpoint.jar.
//
// It does nothing unless the build is darwin/arm64 and the jar exists. A jar that is already patched
// is left alone, so the call is idempotent. A jar the shim does not recognise, or one where a count or
// anchor check fails, is refused with an error before anything is written. A successful patch first
// keeps a one-time backup, Viewpoint.jar.pzpack-mac.bak, beside the jar. It returns the names of the jar
// entries it changed, in jar order, or nil when nothing changed.
func ApplyMacViewpointPatches(modsDir string) ([]string, error) {
	return applyMacViewpointPatches(modsDir, runtime.GOOS, runtime.GOARCH)
}
