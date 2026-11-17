package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pzpack/internal/dist"
	"pzpack/internal/pack"
)

const gb = 1_000_000_000

// freeOf answers free space for the paths in free. A path that is not listed cannot be read.
func freeOf(free map[string]uint64) func(string) (uint64, bool) {
	return func(path string) (uint64, bool) {
		v, ok := free[path]
		return v, ok
	}
}

// separate says that no two paths share a volume.
func separate(a, b string) bool { return false }

// mustRefuse fails unless err is a refusal, is one paragraph, and says each of wants.
func mustRefuse(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the run was not refused")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("refusal is more than one paragraph: %q", err)
	}
	for _, w := range wants {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal %q does not say %q", err, w)
		}
	}
}

func TestPreflightRefusesACacheThatCannotHoldTheRun(t *testing.T) {
	cache, target := t.TempDir(), t.TempDir()
	need := dist.Need{Fetch: 10 * gb, Cache: 10 * gb, Install: gb}
	free := map[string]uint64{cache: 5 * gb, target: 100 * gb}
	err := preflightSpace(need, cache, target, freeOf(free), separate)
	mustRefuse(t, err, "download cache", cache, "10.5 GB", "5.0 GB", "Nothing was downloaded", "--cache")
}

func TestPreflightRefusesAnInstallTargetThatCannotHoldTheProfile(t *testing.T) {
	cache, target := t.TempDir(), t.TempDir()
	need := dist.Need{Cache: gb, Install: 10 * gb}
	free := map[string]uint64{cache: 100 * gb, target: 9 * gb}
	err := preflightSpace(need, cache, target, freeOf(free), separate)
	mustRefuse(t, err, "install target", target, "10.5 GB", "9.0 GB", "Nothing was downloaded", "--root")
}

// The cache and the install target on one volume must both fit: their needs add up.
func TestPreflightAddsBothNeedsOnOneVolume(t *testing.T) {
	cache, target := t.TempDir(), t.TempDir()
	need := dist.Need{Cache: 6 * gb, Install: 6 * gb}
	shared := func(a, b string) bool { return true }
	free := map[string]uint64{cache: 12 * gb}
	mustRefuse(t, preflightSpace(need, cache, target, freeOf(free), shared),
		"also holds", "6.3 GB for the download cache", "6.3 GB for the install", "12.6 GB", "Nothing was downloaded")
	free[cache] = 13 * gb
	if err := preflightSpace(need, cache, target, freeOf(free), shared); err != nil {
		t.Fatalf("13 GB free refused a 12.6 GB run on one volume: %v", err)
	}
}

// Separate volumes are each checked on their own: a roomy cache does not excuse a small install disk.
func TestPreflightChecksSeparateVolumesOnTheirOwn(t *testing.T) {
	cache, target := t.TempDir(), t.TempDir()
	need := dist.Need{Cache: gb, Install: 4 * gb}
	free := map[string]uint64{cache: 2 * gb, target: 2 * gb}
	mustRefuse(t, preflightSpace(need, cache, target, freeOf(free), separate), "install target", "4.2 GB", "2.0 GB")
}

// A volume whose free space cannot be read is not checked, and the readable one still is.
func TestPreflightSkipsAVolumeThatCannotBeRead(t *testing.T) {
	cache, target := t.TempDir(), t.TempDir()
	need := dist.Need{Cache: 100 * gb, Install: 100 * gb}
	if err := preflightSpace(need, cache, target, freeOf(map[string]uint64{}), separate); err != nil {
		t.Fatalf("unknown free space refused the run: %v", err)
	}
	if err := preflightSpace(need, cache, target, freeOf(map[string]uint64{cache: 200 * gb}), separate); err != nil {
		t.Fatalf("the cache has room but the run was refused: %v", err)
	}
}

// A run that needs nothing does not ask the volumes anything.
func TestPreflightDoesNotAskAboutAnEmptyNeed(t *testing.T) {
	asked := func(string) (uint64, bool) {
		t.Fatal("free space was asked for a run that needs nothing")
		return 0, false
	}
	if err := preflightSpace(dist.Need{}, t.TempDir(), t.TempDir(), asked, separate); err != nil {
		t.Fatal(err)
	}
}

// Messages name sizes in the unit that reads best: megabytes, gigabytes or terabytes.
func TestGBTextPicksAUnit(t *testing.T) {
	cases := map[int64]string{
		250_000_000:       "250.0 MB",
		10_500_000_000:    "10.5 GB",
		2_000_000_000_000: "2.0 TB",
	}
	for n, want := range cases {
		if got := gbText(n); got != want {
			t.Errorf("gbText(%d) = %q, want %q", n, got, want)
		}
	}
}

// The headroom is five per cent: 1050 bytes free is enough for a 1000-byte need, 1049 is not.
func TestPreflightMarginIsFivePercent(t *testing.T) {
	cache, target := t.TempDir(), t.TempDir()
	need := dist.Need{Install: 1000}
	if err := preflightSpace(need, cache, target, freeOf(map[string]uint64{target: 1049}), separate); err == nil {
		t.Fatal("1049 bytes free was accepted for a 1000-byte need with 5% headroom")
	}
	if err := preflightSpace(need, cache, target, freeOf(map[string]uint64{target: 1050}), separate); err != nil {
		t.Fatalf("1050 bytes free was refused: %v", err)
	}
}

// Two folders under one temporary directory are on one volume.
func TestSameVolumeSeesTwoFoldersOfOneTempDir(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !sameVolume(a, b) {
		t.Fatal("two folders of one temporary directory were reported on different volumes")
	}
}

// When a volume cannot be named, the answer is yes, so both needs are counted.
func TestSameVolumeCountsBothWhenTheVolumeIsUnknown(t *testing.T) {
	if !sameVolume(filepath.Join(t.TempDir(), "no", "such", "folder"), t.TempDir()) {
		t.Fatal("an unnamed volume was treated as a different volume, which would skip the shared-disk sum")
	}
}

func TestExistingAncestorFindsTheVolumeOfAMissingFolder(t *testing.T) {
	base := t.TempDir()
	if got := existingAncestor(filepath.Join(base, "a", "b", "c")); got != base {
		t.Fatalf("existingAncestor = %q, want %q", got, base)
	}
	if got := existingAncestor(base); got != base {
		t.Fatalf("existingAncestor of an existing folder = %q, want %q", got, base)
	}
}

// The refusal comes from the manifest's own sizes, before the first object is requested.
func TestPreflightRefusesBeforeAnyObjectIsRequested(t *testing.T) {
	sp := serveItemsPack(t, 12, 0, 0)
	cache, target := t.TempDir(), t.TempDir()
	_, m, err := dist.FetchPack(context.Background(), sp.url, cache, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	need := dist.EstimateNeed(m, cache, "lite", false)
	if need.Fetch == 0 || need.Install == 0 {
		t.Fatalf("empty need from a 12-item pack: %+v", need)
	}
	free := map[string]uint64{cache: 1, target: 1}
	mustRefuse(t, preflightSpace(need, cache, target, freeOf(free), separate), "Nothing was downloaded")
	if n := sp.objects.Load(); n != 0 {
		t.Fatalf("%d objects requested before the refusal", n)
	}
}

// The preflight must never accept a run that pack.Install then refuses: both ask pack for the install
// need, and the cache need is the only one main adds the margin to itself.
func TestPreflightInstallNeedMatchesWhatInstallChecks(t *testing.T) {
	for _, n := range []int64{0, 1, 1000, 10_600_000_000} {
		if got, want := pack.InstallSpaceNeed(n), withMargin(n); got != want {
			t.Errorf("pack.InstallSpaceNeed(%d) = %d but the preflight margin gives %d", n, got, want)
		}
	}
}
