package pz

import (
	"fmt"
	"strings"
	"testing"
)

// joinArgs renders JVM args as one space-separated line, for comparing results.
func joinArgs(args []interface{}) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, fmt.Sprint(a))
	}
	return strings.Join(parts, " ")
}

func TestParseSizeMB(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"8192m", 8192, true},
		{"8192M", 8192, true},
		{"8g", 8192, true},
		{"16G", 16384, true},
		{"2048k", 2, true},
		{"1048576", 1, true},
		{"", 0, false},
		{"abc", 0, false},
		{"12x", 0, false},
		{"99999999999999999999g", 0, false},
		{"9999999999999g", 0, false},
	}
	for _, c := range cases {
		got, ok := parseSizeMB(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseSizeMB(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestMaxHeapMB(t *testing.T) {
	if mb, ok := maxHeapMB("-Xmx12288m"); !ok || mb != 12288 {
		t.Fatalf("-Xmx12288m = %d,%v", mb, ok)
	}
	if mb, ok := maxHeapMB("-XX:MaxHeapSize=16g"); !ok || mb != 16384 {
		t.Fatalf("-XX:MaxHeapSize=16g = %d,%v", mb, ok)
	}
	for _, a := range []string{"-Xms2g", "-Dkeep=1", "-XX:InitialHeapSize=2g"} {
		if _, ok := maxHeapMB(a); ok {
			t.Fatalf("%q is not a max-heap argument", a)
		}
	}
}

func TestRewriteVMArgsKeepsLargerUserHeap(t *testing.T) {
	out, note := rewriteVMArgs([]interface{}{"-Xms2g", "-Xmx12288m", "-Dkeep=1"}, 8192, nil)
	if got := joinArgs(out); got != "-Xmx12288m -Dkeep=1" {
		t.Fatalf("args = %q", got)
	}
	if !strings.Contains(note, "kept user -Xmx12288m") || !strings.Contains(note, "-Xmx8192m") {
		t.Fatalf("note = %q", note)
	}
}

func TestRewriteVMArgsReplacesLauncherOwnAndSmallerHeap(t *testing.T) {
	// 10240m is one of the launcher's own sizes, so it is replaced, not kept.
	out, note := rewriteVMArgs([]interface{}{"-Xmx10240m", "-Dkeep=1"}, 8192, nil)
	if got := joinArgs(out); got != "-Dkeep=1 -Xmx8192m" || note != "" {
		t.Fatalf("launcher-owned -Xmx: args=%q note=%q", got, note)
	}
	out, note = rewriteVMArgs([]interface{}{"-Xmx3072m", "-Dkeep=1"}, 8192, nil)
	if got := joinArgs(out); got != "-Dkeep=1 -Xmx8192m" || note != "" {
		t.Fatalf("smaller -Xmx: args=%q note=%q", got, note)
	}
}

func TestRewriteVMArgsLastMaxHeapWins(t *testing.T) {
	// The JVM uses the last -Xmx, so an earlier large value does not count.
	out, note := rewriteVMArgs([]interface{}{"-Xmx12g", "-Xmx2g"}, 8192, nil)
	if got := joinArgs(out); got != "-Xmx8192m" || note != "" {
		t.Fatalf("args=%q note=%q", got, note)
	}
}

func TestRewriteVMArgsDropsOwnAgentAndKeepsOthers(t *testing.T) {
	own := func(s string) bool { return isAgentArg(s, "-javaagent:ZombieBuddy.jar") }
	out, _ := rewriteVMArgs([]interface{}{"-javaagent:ZombieBuddy.jar=verbosity=1", "-XX:MaxHeapSize=2g", "-Dkeep=1", 42}, 8192, own)
	if got := joinArgs(out); got != "-Dkeep=1 42 -Xmx8192m" {
		t.Fatalf("args = %q", got)
	}
}
