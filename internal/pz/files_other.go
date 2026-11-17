//go:build !windows

package pz

// isSharingViolation is always false off Windows: a POSIX rename does not fail
// because another process has the file open.
func isSharingViolation(error) bool { return false }
