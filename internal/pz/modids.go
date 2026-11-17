package pz

import (
	"fmt"
	"strings"
	"unicode"
)

// normalizeModIDs trims each id, drops a leading backslash (RenderDefaultMods has
// always done this), and drops blanks. Callers check the length of the result,
// never of the input, so a list that is blank-only is refused rather than written.
func normalizeModIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(strings.TrimPrefix(id, `\`))
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// validateModIDs refuses ids that contain a control character, because those can
// split or corrupt the line-based files the launcher writes. Internal spaces are
// kept exactly as given. Whether Project Zomboid accepts spaced ids is unverified
// (see the spaced default.txt note in the wiki), so they are neither stripped nor
// rejected.
func validateModIDs(ids []string) error {
	for _, id := range ids {
		for _, r := range id {
			if unicode.IsControl(r) {
				return fmt.Errorf("mod id %q contains control character U+%04X", id, r)
			}
		}
	}
	return nil
}
