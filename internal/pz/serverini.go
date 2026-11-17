package pz

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WriteServerMods rewrites Mods= and WorkshopItems= in an existing server ini.
//
// Everything else is preserved, including a UTF-8 byte-order mark, CRLF line
// endings and the file's permissions. Every Mods= and WorkshopItems= line is
// replaced whatever the spacing around '=', so a duplicated key cannot keep a
// stale value. Blank ids are dropped. Ids containing ';' or a control character
// are refused, because ';' separates the ids in Mods=. The previous file is kept
// as a rotating backup next to it. The game client does not hold the server ini,
// so a change is written even while the game runs.
func WriteServerMods(iniPath string, ids []string) (bool, error) {
	ids = normalizeModIDs(ids)
	if len(ids) == 0 {
		return false, fmt.Errorf("refusing to write an empty mod list")
	}
	if err := validateModIDs(ids); err != nil {
		return false, err
	}
	for _, id := range ids {
		if strings.Contains(id, ";") {
			return false, fmt.Errorf("mod id %q contains ';', which separates ids in Mods=", id)
		}
	}
	raw, err := os.ReadFile(iniPath)
	if err != nil {
		return false, err
	}
	modsLine := "Mods=" + strings.Join(ids, ";")
	workshopLine := "WorkshopItems="

	text := string(raw)
	hasBOM := strings.HasPrefix(text, utf8BOM)
	text = strings.TrimPrefix(text, utf8BOM)
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	// Splitting on "\n" leaves an empty element after a final newline. That is the
	// terminator of the last line, not a line, and keeping it adds a blank line on
	// every rewrite.
	var lines []string
	if text != "" {
		lines = strings.Split(text, "\n")
		if strings.HasSuffix(text, "\n") {
			lines = lines[:len(lines)-1]
		}
	}

	var out []string
	haveMods, haveWorkshop, changed := false, false, false
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		key, _, found := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(key)
		switch {
		case found && key == "Mods":
			if trimmed != modsLine {
				changed = true
			}
			out = append(out, modsLine)
			haveMods = true
		case found && key == "WorkshopItems":
			if trimmed != workshopLine {
				changed = true
			}
			out = append(out, workshopLine)
			haveWorkshop = true
		default:
			out = append(out, line)
		}
	}
	if !haveMods {
		out = append(out, modsLine)
		changed = true
	}
	if !haveWorkshop {
		out = append(out, workshopLine)
		changed = true
	}
	if !changed {
		return false, nil
	}
	if _, err := writeBackup(filepath.Dir(iniPath), filepath.Base(iniPath)+".pzpack", raw); err != nil {
		return false, err
	}
	var body strings.Builder
	if hasBOM {
		body.WriteString(utf8BOM)
	}
	for _, line := range out {
		body.WriteString(line)
		body.WriteString(eol)
	}
	if err := writeFileAtomic(iniPath, []byte(body.String())); err != nil {
		return false, err
	}
	return true, nil
}
