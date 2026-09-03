package gitsource

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	path0 "path"
	"path/filepath"
	"strings"
)

// Limits on what a fetch may unpack.
//
// A job repository is text and small scripts. These are far above anything
// honest and far below anything that fills a disk, and they are checked while
// unpacking rather than after -- a limit enforced after the fact is not a
// limit.
const (
	MaxTotalBytes = 64 << 20 // 64 MB unpacked
	MaxFileBytes  = 16 << 20
	MaxFiles      = 20000
)

// Extracted reports what a fetch unpacked.
type Extracted struct {
	Files int
	Bytes int64

	// SkippedLinks counts symlinks and other non-regular entries left out.
	// Counted rather than ignored: a repository whose scripts are symlinks
	// would otherwise arrive subtly incomplete and fail much later, at run
	// time, with a missing file.
	SkippedLinks int
}

// Extract unpacks a GitHub tarball into dest, safely.
//
// This is the only place in this project that unpacks an archive from outside
// it, so the rules are stated rather than assumed:
//
//   - Every entry's final path must stay inside dest. A tarball containing
//     ../../.ssh/authorized_keys is the oldest trick there is.
//   - Only regular files and directories are written. A symlink is the same
//     escape by another route, since a later entry can be written through it.
//   - Size and count are capped while unpacking.
//
// GitHub wraps everything in a single top-level directory named for the repo
// and commit, which is stripped: a source's root should be the repository's
// root, not owner-repo-a3f81c2/.
func Extract(r io.Reader, dest string) (Extracted, error) {
	var out Extracted

	gz, err := gzip.NewReader(r)
	if err != nil {
		return out, fmt.Errorf("the download is not a gzip stream: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return out, err
	}

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("reading the archive: %w", err)
		}

		rel, ok := stripRoot(header.Name)
		if !ok {
			continue // the wrapper directory itself
		}
		target, err := safeJoin(dest, rel)
		if err != nil {
			return out, err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return out, err
			}
		case tar.TypeReg:
			if out.Files++; out.Files > MaxFiles {
				return out, fmt.Errorf("the repository has more than %d files", MaxFiles)
			}
			if header.Size > MaxFileBytes {
				return out, fmt.Errorf("%s is %d bytes, over the %d byte limit",
					rel, header.Size, MaxFileBytes)
			}
			if out.Bytes += header.Size; out.Bytes > MaxTotalBytes {
				return out, fmt.Errorf("the repository unpacks to more than %d bytes", MaxTotalBytes)
			}
			if err := writeFile(target, tr, header); err != nil {
				return out, err
			}
		default:
			// Symlinks, hardlinks, devices, fifos. Counted so the caller can
			// say how many were left out.
			out.SkippedLinks++
		}
	}
	return out, nil
}

func writeFile(target string, r io.Reader, header *tar.Header) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}

	// The execute bit is carried over and nothing else is: a job repository's
	// scripts have to stay runnable, and setuid or group-writable bits from
	// somebody else's tarball have no business being reproduced here.
	mode := os.FileMode(0o644)
	if header.FileInfo().Mode()&0o100 != 0 {
		mode = 0o755
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer f.Close()

	// Bounded by the header size that was already checked against the caps, so
	// a lying header cannot write more than it declared.
	if _, err := io.Copy(f, io.LimitReader(r, header.Size)); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	return f.Close()
}

// stripRoot removes GitHub's owner-repo-sha/ wrapper directory.
func stripRoot(name string) (string, bool) {
	clean := strings.TrimPrefix(filepath.ToSlash(name), "./")
	slash := strings.Index(clean, "/")
	if slash < 0 {
		return "", false
	}
	rest := clean[slash+1:]
	if rest == "" {
		return "", false
	}
	return rest, true
}

// safeJoin resolves an archive path inside dest, or refuses.
func safeJoin(dest, rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("the archive contains an absolute path: %s", rel)
	}
	target := filepath.Join(dest, filepath.FromSlash(rel))

	// Compared against dest with a separator appended, so that a sibling
	// directory whose name merely starts with dest -- /tmp/x-evil next to
	// /tmp/x -- is not mistaken for a child.
	if target != dest && !strings.HasPrefix(target, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("the archive contains a path that escapes its directory: %s", rel)
	}
	return target, nil
}

// Tar writes dir as a gzipped tarball, wrapped in a single top-level directory
// named root.
//
// The inverse of Extract, and here rather than in a package of its own because
// the two have to agree about a format, and a format agreed across a package
// boundary is one that drifts.
//
// The wrapper is not decoration: GitHub wraps its tarballs the same way and
// Extract strips exactly one leading component. Writing the wrapper means a
// tree served by the control plane and a tree downloaded from GitHub unpack
// through the same code, with the same path-escape rules, rather than through a
// second extractor that has to re-derive them (D25).
//
// Only regular files and directories are written, matching what Extract will
// accept: a symlink that could not survive the round trip is left out here
// rather than discovered as a silent absence there.
func Tar(dir, root string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !d.Type().IsRegular() && !d.IsDir() {
			return nil // symlinks and devices, as above
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = path0.Join(root, filepath.ToSlash(rel))
		if d.IsDir() {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		tw.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
