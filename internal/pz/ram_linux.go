//go:build linux

package pz

// totalRAMBytes reports the memory this process can use on Linux: host MemTotal,
// capped by any cgroup v1 or v2 memory limit (containers and slices).
func totalRAMBytes() uint64 {
	return linuxRAMBytes("/proc/meminfo", "/proc/self/cgroup", "/sys/fs/cgroup")
}
