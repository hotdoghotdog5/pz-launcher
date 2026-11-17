package pz

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// utf8BOM is the byte-order mark some editors put at the start of a text file.
const utf8BOM = "\uFEFF"

// renameAttempts bounds how many times replaceFile tries a rename that failed
// on a transient sharing violation.
const renameAttempts = 5

var (
	// renameFile performs the atomic replace. Tests substitute it.
	renameFile = os.Rename
	// retryableRenameErr reports rename errors worth retrying. Tests substitute it.
	retryableRenameErr = isSharingViolation
	// renameBackoff is the wait before the first retry; it doubles each time,
	// so the worst case is about 1.5 s.
	renameBackoff = 100 * time.Millisecond
)

// readIfExists returns a file's contents and whether it exists. Any read error
// other than "does not exist" is returned, so callers never overwrite a file
// they could not read.
func readIfExists(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return b, true, nil
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return nil, false, err
}

// replaceFile renames tmp over dst. os.Rename replaces an existing file on every
// platform (on Windows it passes MOVEFILE_REPLACE_EXISTING), so dst is never
// deleted first: if the rename fails, dst is left as it was. A transient sharing
// violation (an antivirus scan, or a process briefly holding the file) is retried
// a bounded number of times.
func replaceFile(tmp, dst string) error {
	var err error
	backoff := renameBackoff
	for attempt := 1; attempt <= renameAttempts; attempt++ {
		err = renameFile(tmp, dst)
		if err == nil || !retryableRenameErr(err) {
			return err
		}
		if attempt < renameAttempts {
			time.Sleep(backoff)
			backoff *= 2
		}
	}
	return fmt.Errorf("replace %s: still locked after %d attempts: %w", dst, renameAttempts, err)
}

// writeFileAtomic replaces path with data through a uniquely named temp file in
// the same directory. The existing file's permissions are kept (0644 for a new
// file): a bare temp file is created 0600, which would silently change the mode.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return err
	}
	if err := replaceFile(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
