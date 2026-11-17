package main

import (
	"fmt"
	"os"
	"path/filepath"

	"pzpack/internal/dist"
	"pzpack/internal/pack"
)

// spaceMarginPct is the headroom a run keeps beyond what it needs. It is pack's figure, which Install
// checks with: the preflight must not accept a run that Install then refuses.
const spaceMarginPct = pack.SpaceMarginPct

// preflightSpace refuses a run that a volume cannot hold, before its first object is downloaded. The
// cache needs the largest object in flight plus the items it unpacks, and the install needs the profile's
// bytes; each need carries the headroom. When the cache and the install target share a volume, their
// needs add up. free reports a volume's free bytes, and ok false skips that volume's check. sameVolume
// reports whether two paths sit on one volume. Every refusal says that nothing was downloaded.
func preflightSpace(need dist.Need, cacheDir, target string, free func(string) (uint64, bool), sameVolume func(a, b string) bool) error {
	cacheNeed := withMargin(need.Cache)
	installNeed := pack.InstallSpaceNeed(need.Install)
	if cacheNeed == 0 && installNeed == 0 {
		return nil
	}
	cacheAt, targetAt := existingAncestor(cacheDir), existingAncestor(target)
	if cacheNeed > 0 && installNeed > 0 && sameVolume(cacheAt, targetAt) {
		avail, ok := free(cacheAt)
		if !ok || avail >= uint64(cacheNeed+installNeed) {
			return nil
		}
		return fmt.Errorf("not enough free space on the volume that holds %s, which also holds the install target %s: this run needs about %s for the download cache and %s for the install, %s in all with %d%% headroom, and %s is free. Nothing was downloaded. Free up space on that volume, or pass --cache to a folder on another disk, then run this again.",
			cacheDir, target, gbText(cacheNeed), gbText(installNeed), gbText(cacheNeed+installNeed), spaceMarginPct, gbText(int64(avail)))
	}
	if cacheNeed > 0 {
		if avail, ok := free(cacheAt); ok && avail < uint64(cacheNeed) {
			return fmt.Errorf("not enough free space on the volume that holds the download cache %s: this run needs about %s, with %d%% headroom, and %s is free. Nothing was downloaded. Free up space there, or pass --cache to a folder on a bigger disk, then run this again.",
				cacheDir, gbText(cacheNeed), spaceMarginPct, gbText(int64(avail)))
		}
	}
	if installNeed > 0 {
		if avail, ok := free(targetAt); ok && avail < uint64(installNeed) {
			return fmt.Errorf("not enough free space on the volume that holds the install target %s: this profile needs about %s, with %d%% headroom, and %s is free. Nothing was downloaded. Free up space there, or pass --root to a folder on a bigger disk, then run this again.",
				target, gbText(installNeed), spaceMarginPct, gbText(int64(avail)))
		}
	}
	return nil
}

// withMargin adds the headroom to a need. A need of zero stays zero.
func withMargin(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return n + n*spaceMarginPct/100
}

// gbText formats a byte count for a message: terabytes, gigabytes or megabytes, with one decimal.
func gbText(n int64) string {
	switch {
	case n >= 1e12:
		return fmt.Sprintf("%.1f TB", float64(n)/1e12)
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	}
}

// existingAncestor returns path, or its nearest ancestor that exists: the directory whose volume will hold path.
func existingAncestor(path string) string {
	p, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// sameVolume reports whether two paths may share a volume. When either volume cannot be named the answer
// is yes, so the caller adds the two needs together rather than miss a shared disk.
func sameVolume(a, b string) bool {
	va, okA := pack.VolumeOf(a)
	vb, okB := pack.VolumeOf(b)
	return !okA || !okB || va == vb
}
