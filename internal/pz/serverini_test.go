package pz

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteServerMods(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "servertest.ini")
	original := "PublicName=Test\nMods=Old1;Old2\nWorkshopItems=123;456\nMaxPlayers=8\n"
	if err := os.WriteFile(ini, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := WriteServerMods(ini, []string{"A", "B", "C"})
	if err != nil || !changed {
		t.Fatalf("write: changed=%v err=%v", changed, err)
	}
	body, _ := os.ReadFile(ini)
	s := string(body)
	if !strings.Contains(s, "Mods=A;B;C\n") || !strings.Contains(s, "WorkshopItems=\n") {
		t.Fatalf("unexpected ini:\n%s", s)
	}
	if !strings.Contains(s, "PublicName=Test\n") || !strings.Contains(s, "MaxPlayers=8\n") {
		t.Fatalf("other settings were not preserved:\n%s", s)
	}
	if got := backupContents(t, dir, "servertest.ini.pzpack"); len(got) != 1 || got[0] != original {
		t.Fatalf("backup should hold the original, got %d backups", len(got))
	}

	changed, err = WriteServerMods(ini, []string{"A", "B", "C"})
	if err != nil || changed {
		t.Fatalf("second write should be a no-op: changed=%v err=%v", changed, err)
	}
}

func TestWriteServerModsCRLF(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "server.ini")
	original := "PublicName=Test\r\nMods=Old\r\nWorkshopItems=x\r\n"
	if err := os.WriteFile(ini, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteServerMods(ini, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(ini)
	if !strings.Contains(string(body), "Mods=A;B\r\n") || !strings.Contains(string(body), "WorkshopItems=\r\n") {
		t.Fatalf("CRLF not preserved:\n%q", body)
	}
	if strings.Contains(string(body), "\n") && !strings.Contains(string(body), "\r\n") {
		t.Fatal("expected CRLF endings")
	}
}

func TestWriteServerModsNoExtraBlankLine(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "server.ini")
	if err := os.WriteFile(ini, []byte("PublicName=Test\nMods=Old\nMaxPlayers=8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteServerMods(ini, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ini); string(b) != "PublicName=Test\nMods=A;B\nMaxPlayers=8\nWorkshopItems=\n" {
		t.Fatalf("after first rewrite: %q", b)
	}
	if _, err := WriteServerMods(ini, []string{"C", "D"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ini); string(b) != "PublicName=Test\nMods=C;D\nMaxPlayers=8\nWorkshopItems=\n" {
		t.Fatalf("after second rewrite: %q", b)
	}
}

func TestWriteServerModsBOMSpacedAndDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "server.ini")
	original := "\uFEFFPublicName=Test\nMods =Old\nMods=Other\nWorkshopItems = x\n"
	if err := os.WriteFile(ini, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteServerMods(ini, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	want := "\uFEFFPublicName=Test\nMods=A;B\nMods=A;B\nWorkshopItems=\n"
	if b, _ := os.ReadFile(ini); string(b) != want {
		t.Fatalf("got %q\nwant %q", b, want)
	}
}

func TestWriteServerModsRejectsSemicolonAndControl(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "server.ini")
	orig := "PublicName=Test\nMods=Old\n"
	if err := os.WriteFile(ini, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]string{{"A;B"}, {"Bad\x01Id"}} {
		if _, err := WriteServerMods(ini, ids); err == nil {
			t.Fatalf("ids %q accepted", ids)
		}
	}
	if b, _ := os.ReadFile(ini); string(b) != orig {
		t.Fatal("ini changed by a refused write")
	}
}

func TestWriteServerModsRotatesBackups(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "server.ini")
	if err := os.WriteFile(ini, []byte("PublicName=Test\nMods=Init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 12; i++ {
		if _, err := WriteServerMods(ini, []string{fmt.Sprintf("v%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	got := backupContents(t, dir, "server.ini.pzpack")
	if len(got) != backupKeep {
		t.Fatalf("kept %d backups, want %d", len(got), backupKeep)
	}
	if !strings.Contains(got[len(got)-1], "Mods=v11\n") {
		t.Fatalf("newest backup should hold v11:\n%s", got[len(got)-1])
	}
	if !strings.Contains(got[0], "Mods=v2\n") {
		t.Fatalf("oldest kept backup should hold v2:\n%s", got[0])
	}
}

// The game client does not hold the server ini, so a change is written while the game
// runs. The running check is not consulted for a server ini write, and a backup is
// still taken.
func TestWriteServerModsWritesWhileGameRuns(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "server.ini")
	orig := "PublicName=Test\nMods=A\nWorkshopItems=\n"
	if err := os.WriteFile(ini, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	consulted := 0
	prev := gameRunningCheck
	gameRunningCheck = func() (bool, error) {
		consulted++
		return true, nil
	}
	t.Cleanup(func() { gameRunningCheck = prev })

	changed, err := WriteServerMods(ini, []string{"B"})
	if err != nil || !changed {
		t.Fatalf("change while the game runs: changed=%v err=%v", changed, err)
	}
	if consulted != 0 {
		t.Fatalf("the running check ran %d time(s) for a server ini write", consulted)
	}
	if b, _ := os.ReadFile(ini); string(b) != "PublicName=Test\nMods=B\nWorkshopItems=\n" {
		t.Fatalf("server ini not rewritten:\n%s", b)
	}
	if got := backupContents(t, dir, "server.ini.pzpack"); len(got) != 1 || got[0] != orig {
		t.Fatalf("backup should hold the original, got %d backups", len(got))
	}
}

// A failing running check does not block a server ini write either.
func TestWriteServerModsIgnoresFailingRunningCheck(t *testing.T) {
	dir := t.TempDir()
	ini := filepath.Join(dir, "server.ini")
	if err := os.WriteFile(ini, []byte("Mods=A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setGameRunning(t, false, errors.New("no process lister"))
	if changed, err := WriteServerMods(ini, []string{"B"}); err != nil || !changed {
		t.Fatalf("server ini with a failing check: changed=%v err=%v", changed, err)
	}
}
