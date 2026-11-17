package pz

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// backupContents returns the rotating backups of base in dir, oldest first.
func backupContents(t *testing.T, dir, base string) []string {
	t.Helper()
	names, err := backupNames(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

func TestWriteBackupKeepsNewestTen(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 12; i++ {
		if _, err := writeBackup(dir, "f.txt", []byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	got := backupContents(t, dir, "f.txt")
	if len(got) != backupKeep {
		t.Fatalf("kept %d backups, want %d", len(got), backupKeep)
	}
	if got[0] != "2" || got[len(got)-1] != "11" {
		t.Fatalf("kept %q, want the ten newest (2..11)", got)
	}
}

func TestBackupNamesIgnoreLegacyAndForeignFiles(t *testing.T) {
	dir := t.TempDir()
	keep := []string{"f.txt.bak", "f.txt.mine.bak", "f.txt.notes"}
	for _, n := range keep {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("kept"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		if _, err := writeBackup(dir, "f.txt", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if got := backupContents(t, dir, "f.txt"); len(got) != backupKeep {
		t.Fatalf("kept %d rotating backups, want %d", len(got), backupKeep)
	}
	for _, n := range keep {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s was pruned: %v", n, err)
		}
	}
}

func TestNextStampIsStrictlyIncreasing(t *testing.T) {
	prev := ""
	for i := 0; i < 2000; i++ {
		s := nextStamp()
		if !backupStamp.MatchString(s) {
			t.Fatalf("stamp %q has the wrong shape", s)
		}
		if s <= prev {
			t.Fatalf("stamp %q is not after %q", s, prev)
		}
		prev = s
	}
}

func TestWriteBackupNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	a, err := writeBackup(dir, "x", []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := writeBackup(dir, "x", []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two backups share a name")
	}
	if got, _ := os.ReadFile(a); string(got) != "first" {
		t.Fatalf("first backup was overwritten: %q", got)
	}
}
