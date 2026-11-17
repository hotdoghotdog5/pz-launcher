package pz

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// backupKeep is how many rotating backups are kept per file.
const backupKeep = 10

// maxUniqueAttempts bounds the search for an unused backup or aside name.
const maxUniqueAttempts = 100

// backupStamp matches the UTC timestamp written into a backup name:
// yyyymmddhhmmss.nnnnnnnnn. Fixed width, so lexical order is time order.
var backupStamp = regexp.MustCompile(`^\d{14}\.\d{9}$`)

var (
	stampMu   sync.Mutex
	lastStamp int64
)

// nextStamp returns a UTC timestamp strictly later than every stamp this process
// has handed out. Backups made within one clock tick still sort in creation order.
func nextStamp() string {
	stampMu.Lock()
	defer stampMu.Unlock()
	ns := time.Now().UnixNano()
	if ns <= lastStamp {
		ns = lastStamp + 1
	}
	lastStamp = ns
	return time.Unix(0, ns).UTC().Format("20060102150405.000000000")
}

// writeBackup stores content as a new backup named <base>.<stamp>.bak in dir and
// then prunes the set to backupKeep. The name is created with O_EXCL, so it never
// overwrites an earlier backup. Pruning is best effort: a failure to delete an
// old backup does not fail the write it follows.
func writeBackup(dir, base string, content []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for attempt := 0; attempt < maxUniqueAttempts; attempt++ {
		name := filepath.Join(dir, base+"."+nextStamp()+".bak")
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			os.Remove(name)
			return "", err
		}
		if err := f.Close(); err != nil {
			os.Remove(name)
			return "", err
		}
		pruneBackups(dir, base, backupKeep)
		return name, nil
	}
	return "", fmt.Errorf("could not pick an unused backup name for %s", base)
}

// backupNames lists the rotating backups of base in dir, oldest first. Files that
// do not carry a timestamp (the single pre-rotation <base>.bak from earlier
// versions, or anything else the user put there) are never listed, so they are
// never pruned.
func backupNames(dir, base string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := base + "."
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, prefix) || !strings.HasSuffix(n, ".bak") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(n, prefix), ".bak")
		if backupStamp.MatchString(stamp) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// pruneBackups deletes the oldest rotating backups of base until keep remain.
func pruneBackups(dir, base string, keep int) {
	names, err := backupNames(dir, base)
	if err != nil {
		return
	}
	for len(names) > keep {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}

// newestBackup returns the most recent rotating backup of base in dir.
func newestBackup(dir, base string) (string, bool, error) {
	names, err := backupNames(dir, base)
	if err != nil {
		return "", false, err
	}
	if len(names) == 0 {
		return "", false, nil
	}
	return filepath.Join(dir, names[len(names)-1]), true, nil
}
