package dist

import "path/filepath"

// Need is the disk space a run takes, worked out from the manifest before anything is downloaded.
type Need struct {
	Fetch   int64 // bytes the run downloads: the objects of the items that are not complete in the cache
	Cache   int64 // peak bytes the cache holds: the largest object in flight plus the items already extracted
	Install int64 // bytes the profile installs into the mods folder: every item the profile needs
}

// EstimateNeed returns what a profile needs from the disk. An item whose key is already complete in
// cacheDir is neither downloaded nor extracted again, so it adds only to Install. A refresh fetches
// every item again. A layer manifest keeps each archive beside the tree it extracts, so its cache need
// is twice its archives, counted as if none of them were cached.
func EstimateNeed(m *Manifest, cacheDir, profile string, refresh bool) Need {
	var n Need
	if m == nil {
		return n
	}
	if m.ItemsSchema != 2 {
		names := []string{"base"}
		if profile == "full" {
			names = append(names, "client")
		}
		for _, name := range names {
			if l, ok := m.Layers[name]; ok {
				n.Fetch += l.Bytes
				n.Install += l.Bytes
			}
		}
		n.Cache = 2 * n.Fetch
		return n
	}
	var extracted, largest int64
	itemsDir := filepath.Join(cacheDir, "items")
	for _, it := range neededItems(m, profile) {
		n.Install += it.Bytes
		if !refresh && itemComplete(filepath.Join(itemsDir, it.TreeSHA256), it.Object.SHA256) {
			continue
		}
		n.Fetch += it.Object.Bytes
		extracted += it.Bytes
		if it.Object.Bytes > largest {
			largest = it.Object.Bytes
		}
	}
	n.Cache = largest + extracted
	return n
}
