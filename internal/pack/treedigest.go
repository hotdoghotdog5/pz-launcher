package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// fileSum is one entry of a tree digest: a file's path relative to the tree root (with
// forward slashes) and the hex SHA-256 of its contents.
type fileSum struct {
	rel string
	sum string
}

// digestSums returns the tree digest: SHA-256 over the sorted "<relpath>\t<sha256>\n"
// lines. That is the algorithm pz-server-modpack/tools/verify_manifest.py uses for
// tree_sha256, so a tree hashed here matches the publisher's hash of the same files.
func digestSums(sums []fileSum) string {
	sort.Slice(sums, func(i, j int) bool { return sums[i].rel < sums[j].rel })
	h := sha256.New()
	for _, s := range sums {
		fmt.Fprintf(h, "%s\t%s\n", s.rel, s.sum)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// hashFile returns the hex SHA-256 of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// walkTree calls visit for every non-directory entry under root that the installer copies.
// Dot-entries are skipped exactly as copyDir skips them. The root itself is not visited.
func walkTree(root string, visit func(rel, path string, info fs.FileInfo) error) error {
	return filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skipDotted(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		return visit(filepath.ToSlash(rel), path, info)
	})
}

// treeDigest returns the tree digest of the regular files under root. Symlinks are left
// out, the same way copyDir leaves them out of the digest it returns.
func treeDigest(root string) (string, error) {
	var sums []fileSum
	err := walkTree(root, func(rel, path string, info fs.FileInfo) error {
		if !info.Mode().IsRegular() {
			return nil
		}
		sum, err := hashFile(path)
		if err != nil {
			return err
		}
		sums = append(sums, fileSum{rel: rel, sum: sum})
		return nil
	})
	if err != nil {
		return "", err
	}
	return digestSums(sums), nil
}

// treeStats is the file count and byte total the installer copies from a tree.
type treeStats struct {
	files int64
	bytes int64
}

// statTree counts the non-directory entries under root that the installer copies. It reads
// metadata only, never file contents.
func statTree(root string) (treeStats, error) {
	var st treeStats
	err := walkTree(root, func(_, _ string, info fs.FileInfo) error {
		st.files++
		st.bytes += info.Size()
		return nil
	})
	return st, err
}
