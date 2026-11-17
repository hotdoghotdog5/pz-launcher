//go:build windows

package pack

import (
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// getDiskFreeSpaceEx is kernel32's GetDiskFreeSpaceExW. Its first output is the bytes the calling user
// may use on the volume that holds a directory, the same quota-aware figure the copy needs.
var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// freeBytes reports the bytes the current user can still write on the volume that holds path. ok is
// false when the query fails; the caller then skips the space check.
func freeBytes(path string) (uint64, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, false
	}
	dir, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return 0, false
	}
	var avail, total, totalFree uint64
	ok, _, _ := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(dir)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ok == 0 {
		return 0, false
	}
	return avail, true
}

// VolumeOf names the volume that holds path by its drive letter or UNC share. ok is false when the path
// names no volume, so the caller must assume that two paths may share one.
func VolumeOf(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	v := filepath.VolumeName(abs)
	if v == "" {
		return "", false
	}
	return strings.ToUpper(v), true
}
