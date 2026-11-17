package pz

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupUnlimited is above any real memory size. cgroup v1 reports about 2^63
// when no limit is set, and such values are ignored.
const cgroupUnlimited = uint64(1) << 60

// linuxRAMBytes is the memory this process can really use: the smaller of host
// MemTotal and every cgroup memory limit in effect. A containerised process with a
// 6 GiB limit must not size its heap for the host's RAM. Missing or unreadable
// sources are skipped, and 0 means unknown.
func linuxRAMBytes(meminfoPath, procCgroupPath, cgroupRoot string) uint64 {
	total := readMemTotal(meminfoPath)
	for _, limit := range cgroupMemoryLimits(procCgroupPath, cgroupRoot) {
		if total == 0 || limit < total {
			total = limit
		}
	}
	return total
}

// readMemTotal returns MemTotal from a meminfo file, in bytes, or 0.
func readMemTotal(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}

// cgroupMemoryLimits reads every memory limit that applies to this process. It
// checks the cgroup v2 memory.max and the cgroup v1 memory.limit_in_bytes, at the
// root of the hierarchy and at the process's own cgroup path from procCgroupPath.
// The effective limit is the smallest of these, so all of them are returned.
func cgroupMemoryLimits(procCgroupPath, root string) []uint64 {
	files := []string{
		filepath.Join(root, "memory.max"),
		filepath.Join(root, "memory", "memory.limit_in_bytes"),
	}
	if data, err := os.ReadFile(procCgroupPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
			if len(parts) != 3 {
				continue
			}
			id, controllers, path := parts[0], parts[1], parts[2]
			switch {
			case id == "0" && controllers == "":
				files = append(files, filepath.Join(root, path, "memory.max"))
			case containsController(controllers, "memory"):
				files = append(files, filepath.Join(root, "memory", path, "memory.limit_in_bytes"))
			}
		}
	}
	var limits []uint64
	for _, f := range files {
		if n, ok := readCgroupLimit(f); ok {
			limits = append(limits, n)
		}
	}
	return limits
}

// containsController reports whether a cgroup v1 controller list (such as
// "cpu,cpuacct") names the controller.
func containsController(list, name string) bool {
	for _, c := range strings.Split(list, ",") {
		if c == name {
			return true
		}
	}
	return false
}

// readCgroupLimit reads one limit file. "max" (v2, unlimited) and absurdly large
// values (v1, unlimited) report false, as do unreadable files.
func readCgroupLimit(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n == 0 || n >= cgroupUnlimited {
		return 0, false
	}
	return n, true
}
