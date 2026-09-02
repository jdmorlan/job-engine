package selfupdate

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// maxDownload bounds what we will pull down. The binary is around 11MB; this
// leaves generous room while making a redirect to something enormous a bounded
// failure rather than a full disk.
const maxDownload = 200 << 20

// Download fetches a URL to a file in dir and returns its path and SHA-256.
//
// The hash is computed while streaming rather than by re-reading afterwards,
// so the bytes that were verified are provably the bytes that were written.
func Download(ctx context.Context, client *http.Client, url, dir, name string) (path, sum string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("downloading %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("downloading %s: %s", name, resp.Status)
	}

	f, err := os.CreateTemp(dir, ".je-download-*")
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), io.LimitReader(resp.Body, maxDownload)); err != nil {
		os.Remove(f.Name())
		return "", "", fmt.Errorf("downloading %s: %w", name, err)
	}
	return f.Name(), hex.EncodeToString(hasher.Sum(nil)), nil
}

// ParseChecksums reads a `sha256sum`-style manifest into name -> hash.
func ParseChecksums(r io.Reader) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		// sha256sum writes "<hash>  <name>" and prefixes the name with * for
		// binary mode; accept either.
		sums[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(sums) == 0 {
		return nil, errors.New("checksum file is empty")
	}
	return sums, nil
}

// ExtractBinary pulls a single named file out of a .tar.gz.
//
// Entry paths are checked rather than trusted. This is the one place in the
// program that unpacks an archive fetched from the network, and an entry named
// ../../.ssh/authorized_keys is the oldest trick there is.
func ExtractBinary(archivePath, want, destDir string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("reading archive: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("archive does not contain %q", want)
		}
		if err != nil {
			return "", fmt.Errorf("reading archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		// Compare the base name only, and never join the archive's own path
		// into a destination.
		if filepath.Base(header.Name) != want {
			continue
		}

		out, err := os.CreateTemp(destDir, ".je-extract-*")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, maxDownload)); err != nil {
			out.Close()
			os.Remove(out.Name())
			return "", err
		}
		if err := out.Close(); err != nil {
			return "", err
		}
		if err := os.Chmod(out.Name(), 0o755); err != nil {
			return "", err
		}
		return out.Name(), nil
	}
}

// Replace atomically swaps the binary at target for the one at replacement.
//
// Rename rather than write-in-place, and the difference is not stylistic:
// writing into a running executable fails outright on Linux (ETXTBSY) and
// produces a corrupt binary elsewhere. Renaming over it leaves the running
// process on the old inode, happily finishing whatever it was doing, while
// every new invocation gets the new file.
//
// The replacement must already be in the target's directory, since rename
// cannot cross filesystems.
func Replace(target, replacement string) error {
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("locating the current binary: %w", err)
	}
	// Preserve whatever mode the existing install has, rather than imposing
	// 0755 on something deliberately tightened.
	if err := os.Chmod(replacement, info.Mode().Perm()); err != nil {
		return err
	}
	if err := os.Rename(replacement, target); err != nil {
		return fmt.Errorf("replacing %s: %w", target, err)
	}
	return nil
}

// CurrentBinary reports the path of the running executable, resolved through
// any symlinks so an upgrade replaces the real file rather than the link.
func CurrentBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return exe, nil // not fatal; the unresolved path is still usable
	}
	return resolved, nil
}

// ManagedElsewhere reports that this binary is owned by something that should
// do the upgrading, and says what.
//
// Replacing a Homebrew-managed file works, right up until the next `brew
// upgrade` silently reverts it and the version you thought you were running is
// not the one you are.
func ManagedElsewhere(path string) string {
	switch {
	case strings.Contains(path, "/Cellar/"), strings.Contains(path, "/homebrew/"):
		return "Homebrew (use: brew upgrade je)"
	case strings.Contains(path, "/nix/store/"):
		return "Nix"
	case strings.Contains(path, "/go-build"), strings.Contains(path, os.TempDir()+"/go-"):
		return "go run (build and install it properly first)"
	}
	return ""
}

// Writable reports whether the current user can replace a file, which needs
// write permission on its *directory* rather than on the file itself.
func Writable(path string) bool {
	dir := filepath.Dir(path)
	probe, err := os.CreateTemp(dir, ".je-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return true
}
