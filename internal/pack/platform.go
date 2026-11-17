package pack

import (
	"fmt"
	"slices"
	"strings"
)

// knownPlatforms maps each GOOS to the GOARCH values the Go toolchain builds for it. It is the
// output of `go tool dist list` for Go 1.27.3. Refresh it when the toolchain changes. An entry in
// an item's platforms list must name one of these OS values, or one of these GOOS/GOARCH pairs.
var knownPlatforms = map[string][]string{
	"aix":       {"ppc64"},
	"android":   {"386", "amd64", "arm", "arm64"},
	"darwin":    {"amd64", "arm64"},
	"dragonfly": {"amd64"},
	"freebsd":   {"386", "amd64", "arm", "arm64"},
	"illumos":   {"amd64"},
	"ios":       {"amd64", "arm64"},
	"js":        {"wasm"},
	"linux":     {"386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le", "mipsle", "ppc64", "ppc64le", "riscv64", "s390x"},
	"netbsd":    {"386", "amd64", "arm", "arm64"},
	"openbsd":   {"386", "amd64", "arm", "arm64", "ppc64", "riscv64"},
	"plan9":     {"386", "amd64", "arm"},
	"solaris":   {"amd64"},
	"wasip1":    {"wasm"},
	"windows":   {"386", "amd64", "arm64"},
}

// checkPlatform accepts one platforms entry: a bare "GOOS" or a "GOOS/GOARCH" pair that the Go
// toolchain builds for. Matching is exact and case-sensitive, so "Darwin/ARM64" is refused.
func checkPlatform(entry string) error {
	goos, goarch, pair := strings.Cut(entry, "/")
	archs, ok := knownPlatforms[goos]
	if !ok {
		return fmt.Errorf("platform %q: unknown OS %q (want GOOS or GOOS/GOARCH, e.g. darwin/arm64)", entry, goos)
	}
	if pair && !slices.Contains(archs, goarch) {
		return fmt.Errorf("platform %q: %s has no %q build", entry, goos, goarch)
	}
	return nil
}

// platformMatches reports whether an item gated by platforms applies to goos/goarch. An empty list
// applies everywhere. An entry is "GOOS/GOARCH", which needs both to match, or a bare "GOOS".
// The inputs are the build's GOOS and GOARCH, so tests can ask about any platform.
func platformMatches(platforms []string, goos, goarch string) bool {
	if len(platforms) == 0 {
		return true
	}
	for _, p := range platforms {
		if p == goos || p == goos+"/"+goarch {
			return true
		}
	}
	return false
}

// ItemOnPlatform reports whether an item applies to goos/goarch. An item with no platforms applies
// everywhere. Callers that walk pack items directly (outside Selected) must use this, or they will
// require sources the running build never installs.
func ItemOnPlatform(it Item, goos, goarch string) bool {
	return platformMatches(it.Platforms, goos, goarch)
}
