package dist

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ExtractTarGz unpacks a layer archive into dest.
//
// Only regular files and directories under mods/ are accepted. Absolute paths, "..", symlinks and
// hardlinks are rejected, so an archive can never write outside dest. Extraction also refuses to
// write through a symlink that already sits in dest, and it replaces a file left behind by an
// earlier extraction, read-only or not.
func ExtractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	magic := make([]byte, 2)
	if n, _ := io.ReadFull(f, magic); n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	} else if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	cleanDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(hdr.Name)
		name = strings.TrimPrefix(name, "./")
		if name == "" || name == "." {
			continue
		}
		if strings.HasPrefix(name, "/") {
			return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		for _, seg := range strings.Split(name, "/") {
			if seg == ".." {
				return fmt.Errorf("unsafe path in archive: %q", hdr.Name)
			}
		}
		if !strings.HasPrefix(name, "mods/") {
			return fmt.Errorf("unexpected path in archive: %q", hdr.Name)
		}
		target := filepath.Join(cleanDest, filepath.FromSlash(name))
		if !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) {
			return fmt.Errorf("path escapes destination: %q", hdr.Name)
		}
		if err := refuseSymlinks(cleanDest, target); err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			// Replace what an earlier extraction left, read-only files included. O_EXCL makes
			// the create fail rather than follow a link.
			if fi, err := os.Lstat(target); err == nil {
				if fi.IsDir() {
					return fmt.Errorf("archive file %q collides with a folder", hdr.Name)
				}
				if err := os.Remove(target); err != nil {
					return err
				}
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
}

// refuseSymlinks returns an error when a component of target below root is a symbolic link.
// Writing through one would put the file wherever the link points. The root itself is not
// checked: a cache folder may legitimately sit behind a link.
func refuseSymlinks(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	cur := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil // nothing below this point exists yet
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to extract through symlink %s", cur)
		}
	}
	return nil
}
