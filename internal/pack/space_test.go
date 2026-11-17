package pack

import (
	"os"
	"path/filepath"
	"testing"
)

// Two folders of one temporary directory name the same volume, on every platform that can name volumes.
func TestVolumeOfNamesTheVolumeOfAFolder(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	va, okA := VolumeOf(a)
	if !okA {
		t.Skip("this platform cannot name the volume of a folder")
	}
	vb, okB := VolumeOf(b)
	if !okB || va != vb {
		t.Fatalf("VolumeOf(a) = %q, %v; VolumeOf(b) = %q, %v: two folders of one directory must share a volume", va, okA, vb, okB)
	}
}

// FreeBytes answers through the same query the install uses, so a test that replaces the query sees it here.
func TestFreeBytesWrapperUsesTheInjectableQuery(t *testing.T) {
	old := freeBytesFn
	freeBytesFn = func(string) (uint64, bool) { return 42, true }
	t.Cleanup(func() { freeBytesFn = old })
	if free, ok := FreeBytes(t.TempDir()); !ok || free != 42 {
		t.Fatalf("FreeBytes = %d, %v; want the injected 42, true", free, ok)
	}
}

// Install and the preflight in main share one rule. The need is the pack plus SpaceMarginPct, so 1050 bytes
// free is enough for a 1000-byte pack and 1049 is not. The old rule asked for three times the pack, after the
// download had filled the same volume, and refused runs that had room.
func TestInstallSpaceNeedIsThePackPlusTheMargin(t *testing.T) {
	for _, c := range []struct{ total, want int64 }{{0, 0}, {-5, 0}, {1000, 1050}, {10_600_000_000, 11_130_000_000}} {
		if got := InstallSpaceNeed(c.total); got != c.want {
			t.Errorf("InstallSpaceNeed(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}

func TestCheckSpaceAcceptsAPackPlusMarginAndRefusesLess(t *testing.T) {
	old := freeBytesFn
	t.Cleanup(func() { freeBytesFn = old })
	opt := InstallOptions{Target: t.TempDir()}
	for _, c := range []struct {
		free uint64
		ok   bool
	}{{1049, false}, {1050, true}, {3000, true}} {
		free := c.free
		freeBytesFn = func(string) (uint64, bool) { return free, true }
		err := checkSpace(Plan{}, opt, "", map[string]bool{}, 1000)
		if c.ok && err != nil {
			t.Errorf("%d bytes free for a 1000-byte pack was refused: %v", free, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%d bytes free for a 1000-byte pack was accepted; the margin is %d%%", free, SpaceMarginPct)
		}
	}
	// A volume that cannot be asked skips the check, as before.
	freeBytesFn = func(string) (uint64, bool) { return 0, false }
	if err := checkSpace(Plan{}, opt, "", map[string]bool{}, 1000); err != nil {
		t.Errorf("an unreadable volume must skip the check, got %v", err)
	}
}
