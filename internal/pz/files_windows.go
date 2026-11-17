//go:build windows

package pz

import (
	"errors"
	"syscall"
)

// Win32 error codes from winerror.h. The syscall package does not export these names.
const (
	errorSharingViolation = syscall.Errno(32) // ERROR_SHARING_VIOLATION
	errorLockViolation    = syscall.Errno(33) // ERROR_LOCK_VIOLATION
)

// isSharingViolation reports the transient errors a rename hits while another
// process holds the file open.
func isSharingViolation(err error) bool {
	return errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation)
}
