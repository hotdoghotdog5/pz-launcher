package pz

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestFile writes body to path, creating parent directories first.
func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sixteenGiBMeminfo = "MemTotal:       16777216 kB\nMemFree:         1024 kB\n"

// ramFixture writes the meminfo (unless empty) and cgroup files and returns their
// paths, plus an empty cgroup root for the test to fill in.
func ramFixture(t *testing.T, meminfo, procCgroup string) (meminfoPath, cgPath, root string) {
	t.Helper()
	dir := t.TempDir()
	meminfoPath = filepath.Join(dir, "meminfo")
	if meminfo != "" {
		writeTestFile(t, meminfoPath, meminfo)
	}
	cgPath = filepath.Join(dir, "cgroup")
	writeTestFile(t, cgPath, procCgroup)
	root = filepath.Join(dir, "sys")
	return meminfoPath, cgPath, root
}

func TestLinuxRAMUsesHostMemoryWithoutLimits(t *testing.T) {
	meminfo, cg, root := ramFixture(t, sixteenGiBMeminfo, "0::/\n")
	if got, want := linuxRAMBytes(meminfo, cg, root), uint64(16)<<30; got != want {
		t.Fatalf("ram = %d, want %d", got, want)
	}
}

func TestLinuxRAMCappedByCgroupV2Limit(t *testing.T) {
	meminfo, cg, root := ramFixture(t, sixteenGiBMeminfo, "0::/docker/abc\n")
	writeTestFile(t, filepath.Join(root, "memory.max"), "max\n")
	writeTestFile(t, filepath.Join(root, "docker", "abc", "memory.max"), "6442450944\n")
	if got, want := linuxRAMBytes(meminfo, cg, root), uint64(6)<<30; got != want {
		t.Fatalf("ram = %d, want %d (6 GiB cgroup v2 limit)", got, want)
	}
}

func TestLinuxRAMCappedByCgroupV1Limit(t *testing.T) {
	meminfo, cg, root := ramFixture(t, sixteenGiBMeminfo, "4:memory:/lab\n3:cpu,cpuacct:/lab\n")
	// The root v1 file reports the unlimited sentinel; the process's own cgroup has the limit.
	writeTestFile(t, filepath.Join(root, "memory", "memory.limit_in_bytes"), "9223372036854771712\n")
	writeTestFile(t, filepath.Join(root, "memory", "lab", "memory.limit_in_bytes"), "3221225472\n")
	if got, want := linuxRAMBytes(meminfo, cg, root), uint64(3)<<30; got != want {
		t.Fatalf("ram = %d, want %d (3 GiB cgroup v1 limit)", got, want)
	}
}

func TestLinuxRAMIgnoresLimitsAboveHostMemory(t *testing.T) {
	meminfo, cg, root := ramFixture(t, sixteenGiBMeminfo, "0::/\n")
	writeTestFile(t, filepath.Join(root, "memory.max"), "68719476736\n") // 64 GiB
	if got, want := linuxRAMBytes(meminfo, cg, root), uint64(16)<<30; got != want {
		t.Fatalf("ram = %d, want %d (host memory, not the larger limit)", got, want)
	}
}

func TestLinuxRAMUsesLimitWhenMeminfoUnreadable(t *testing.T) {
	meminfo, cg, root := ramFixture(t, "", "0::/x\n")
	writeTestFile(t, filepath.Join(root, "x", "memory.max"), "2147483648\n")
	if got, want := linuxRAMBytes(meminfo, cg, root), uint64(2)<<30; got != want {
		t.Fatalf("ram = %d, want %d", got, want)
	}
}

func TestLinuxRAMUnknownWhenNothingReadable(t *testing.T) {
	meminfo, cg, root := ramFixture(t, "", "0::/\n")
	if got := linuxRAMBytes(meminfo, cg, root); got != 0 {
		t.Fatalf("ram = %d, want 0 (unknown)", got)
	}
}
