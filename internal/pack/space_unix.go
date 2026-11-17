//go:build darwin || linux || freebsd

package pack

import (
	"strconv"
	"syscall"
)

// freeBytes reports the bytes an unprivileged user can still write on the volume that holds
// path. ok is false when the query fails; the caller then skips the space check.
func freeBytes(path string) (free uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	if float64(st.Bavail) <= 0 {
		return 0, true
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}

// VolumeOf names the volume that holds path by its device number. Two paths share a volume exactly
// when their names are equal. ok is false when path cannot be stat'ed.
func VolumeOf(path string) (string, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return "", false
	}
	return strconv.FormatUint(uint64(st.Dev), 10), true
}
