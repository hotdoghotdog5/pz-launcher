package dist

import (
	"fmt"
	"os"
	"path/filepath"
)

// rebuildSource extracts the archives into a fresh folder beside srcDir, records the layer set in
// it, and swaps it in with two renames. The previous tree is removed only once the new one is in
// place, so an interrupted rebuild leaves either the old tree or the new one.
func rebuildSource(srcDir string, archives []string, set string) error {
	stage := srcDir + ".staging"
	previous := srcDir + ".previous"
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	for _, archive := range archives {
		if err := ExtractTarGz(archive, stage); err != nil {
			return fmt.Errorf("extract %s: %w", filepath.Base(archive), err)
		}
	}
	if err := os.MkdirAll(filepath.Join(stage, ".pzpack"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, ".pzpack", "set"), []byte(set), 0o644); err != nil {
		return err
	}
	if err := os.RemoveAll(previous); err != nil {
		return err
	}
	if _, err := os.Stat(srcDir); err == nil {
		if err := os.Rename(srcDir, previous); err != nil {
			return err
		}
	}
	if err := os.Rename(stage, srcDir); err != nil {
		return err
	}
	_ = os.RemoveAll(previous)
	return nil
}
