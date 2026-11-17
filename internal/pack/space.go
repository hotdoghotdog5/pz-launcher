package pack

// SpaceMarginPct is the headroom, in per cent, that a run keeps beyond the bytes it writes. The preflight
// before any download and the check inside Install use the same figure, so that one never accepts a run
// the other refuses.
const SpaceMarginPct = 5

// InstallSpaceNeed is the free space Install wants on the target volume for a profile of total bytes: the
// bytes it copies plus the headroom. Folders moved aside are added by Install, which alone can see them.
func InstallSpaceNeed(total int64) int64 {
	if total <= 0 {
		return 0
	}
	return total + total*SpaceMarginPct/100
}

// FreeBytes reports the bytes the current user can still write on the volume that holds path. ok is
// false when the volume cannot be asked; the caller then skips its own check.
func FreeBytes(path string) (uint64, bool) {
	return freeBytesFn(path)
}
