package pz

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultModsPath is the file the game reads for the main-menu / single-player
// mod list. Its entry order is the load order.
func DefaultModsPath(zomboidDir string) string {
	return filepath.Join(zomboidDir, "mods", "default.txt")
}

// defaultBackupDir holds the rotating backups of default.txt.
func defaultBackupDir(zomboidDir string) string {
	return filepath.Join(zomboidDir, "mods", ".pzpack")
}

// RenderDefaultMods produces the exact format Project Zomboid writes itself.
// Blank ids are dropped (see normalizeModIDs).
func RenderDefaultMods(ids []string) string {
	var b strings.Builder
	b.WriteString("VERSION = 1,\n\nmods\n{\n")
	for _, id := range normalizeModIDs(ids) {
		b.WriteString("    mod = ")
		b.WriteString(id)
		b.WriteString(",\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// WriteDefaultMods replaces Zomboid/mods/default.txt with exactly ids, in order.
//
// Blank ids are dropped before the length check, so a list that is blank-only is
// refused instead of being written over the existing file. Each change first keeps
// the previous file as a rotating backup (see backup.go). It returns changed=false
// when the file already matches. While the game is running, a write that would
// change the file is refused with ErrGameRunning.
func WriteDefaultMods(zomboidDir string, ids []string) (bool, error) {
	ids = normalizeModIDs(ids)
	if len(ids) == 0 {
		return false, fmt.Errorf("refusing to write an empty mod list")
	}
	if err := validateModIDs(ids); err != nil {
		return false, err
	}
	path := DefaultModsPath(zomboidDir)
	want := []byte(RenderDefaultMods(ids))

	cur, existed, err := readIfExists(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w (refusing to overwrite a file that cannot be read)", path, err)
	}
	if existed && bytes.Equal(cur, want) {
		return false, nil
	}
	if err := guardGameWrite(true); err != nil {
		return false, err
	}
	if existed {
		if _, err := writeBackup(defaultBackupDir(zomboidDir), filepath.Base(path), cur); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if err := writeFileAtomic(path, want); err != nil {
		return false, err
	}
	return true, nil
}

// RestoreDefaultMods puts the newest backup of default.txt back. A single backup
// from earlier launcher versions (default.txt.bak) is used when no rotating backup
// exists. It returns false when there is nothing to restore.
func RestoreDefaultMods(zomboidDir string) (bool, error) {
	dir := defaultBackupDir(zomboidDir)
	src, ok, err := newestBackup(dir, "default.txt")
	if err != nil {
		return false, err
	}
	if !ok {
		legacy := filepath.Join(dir, "default.txt.bak")
		if _, err := os.Stat(legacy); err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
		src = legacy
	}
	want, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	path := DefaultModsPath(zomboidDir)
	cur, existed, err := readIfExists(path)
	if err != nil {
		return false, err
	}
	if existed && bytes.Equal(cur, want) {
		return true, nil
	}
	if err := guardGameWrite(true); err != nil {
		return false, err
	}
	if err := writeFileAtomic(path, want); err != nil {
		return false, err
	}
	return true, nil
}
