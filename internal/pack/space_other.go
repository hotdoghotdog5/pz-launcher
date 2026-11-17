//go:build !darwin && !linux && !freebsd && !windows

package pack

// freeBytes is not implemented on this platform. ok is false, so the space check is skipped and a
// full disk still fails the copy with the OS error. Windows has its own implementation in
// space_windows.go. The build tags of this file and space_unix.go cover every GOOS exactly once.
func freeBytes(path string) (uint64, bool) {
	return 0, false
}

// VolumeOf cannot name the volume on this platform. ok is false, so the caller must assume that two
// paths may share one.
func VolumeOf(path string) (string, bool) {
	return "", false
}
