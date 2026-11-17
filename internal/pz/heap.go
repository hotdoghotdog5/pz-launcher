package pz

import (
	"fmt"
	"strconv"
	"strings"
)

// Heap sizes the launcher writes, in MB.
const (
	heapSmallMB = 8192
	heapLargeMB = 10240
)

// isLauncherHeapMB reports whether mb is one of the sizes this launcher writes. A
// -Xmx of exactly that size is treated as ours, so it is replaced rather than kept
// as if the user had chosen it. Without this, a value written on a large machine
// would survive forever on a smaller one (or in a container with a memory limit).
func isLauncherHeapMB(mb int) bool {
	return mb == heapSmallMB || mb == heapLargeMB
}

// rewriteVMArgs drops the JVM args the launcher owns (heap sizes, and any entry
// ownAgent matches) and appends the computed -Xmx. A user -Xmx larger than heapMB
// is kept instead, unless it is one of the launcher's own sizes. Only the last
// max-heap argument counts, because that is the one the JVM uses. The returned
// note describes a kept value ("" when the computed -Xmx was written).
func rewriteVMArgs(args []interface{}, heapMB int, ownAgent func(string) bool) ([]interface{}, string) {
	keepIdx, keepMB := -1, 0
	for i, a := range args {
		if mb, ok := maxHeapMB(asString(a)); ok {
			keepIdx, keepMB = i, mb
		}
	}
	keep := keepIdx >= 0 && keepMB > heapMB && !isLauncherHeapMB(keepMB)

	out := make([]interface{}, 0, len(args)+2)
	for i, a := range args {
		if keep && i == keepIdx {
			out = append(out, a)
			continue
		}
		s := asString(a)
		if isHeapArg(s) || (ownAgent != nil && ownAgent(s)) {
			continue
		}
		out = append(out, a)
	}
	if keep {
		return out, fmt.Sprintf("kept user %s (above computed -Xmx%dm)", asString(args[keepIdx]), heapMB)
	}
	return append(out, fmt.Sprintf("-Xmx%dm", heapMB)), ""
}

// asString returns the value of a JVM arg, or "" for a non-string element.
func asString(a interface{}) string {
	s, _ := a.(string)
	return s
}

// maxHeapMB returns the size in MB of a -Xmx or -XX:MaxHeapSize argument.
func maxHeapMB(a string) (int, bool) {
	switch {
	case strings.HasPrefix(a, "-Xmx"):
		return parseSizeMB(strings.TrimPrefix(a, "-Xmx"))
	case strings.HasPrefix(a, "-XX:MaxHeapSize="):
		return parseSizeMB(strings.TrimPrefix(a, "-XX:MaxHeapSize="))
	}
	return 0, false
}

// parseSizeMB parses a JVM size (bytes, or a k, m or g suffix) into whole MB,
// rounding down. It reports false for anything it cannot read.
func parseSizeMB(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	unit := v[len(v)-1]
	digits := v
	switch unit {
	case 'k', 'K', 'm', 'M', 'g', 'G':
		digits = v[:len(v)-1]
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	var mb uint64
	switch unit {
	case 'k', 'K':
		mb = n >> 10
	case 'm', 'M':
		mb = n
	case 'g', 'G':
		if n > 1<<40 {
			return 0, false
		}
		mb = n << 10
	default:
		mb = n >> 20
	}
	if mb > 1<<30 {
		return 0, false
	}
	return int(mb), true
}
