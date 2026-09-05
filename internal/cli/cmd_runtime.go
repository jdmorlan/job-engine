package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jdmorlan/job-engine/internal/selfupdate"
	"github.com/jdmorlan/job-engine/internal/toolchain"
)

// `je worker runtime` -- what this machine can prepare, and getting what it
// cannot (D28).
//
// Installation is explicit and never implicit. A worker does not fetch a
// compiler because a job turned up: the CLI owns the work so nobody is told to
// go and run somebody else's installer (D26), and it does not do it behind
// anybody's back on a machine they thought they understood (D27).

func runWorkerRuntimes(ctx context.Context, env *Env) error {
	lookPath := toolchainLookPath(env)
	available := map[string]bool{}
	for _, name := range toolchain.Available(lookPath) {
		available[name] = true
	}

	st := env.Style
	tw := env.table()
	fmt.Fprintln(tw, st.Header("LANGUAGE\tTOOL\tSTATUS"))
	for _, name := range toolchain.Names() {
		tc, _ := toolchain.Lookup(name)
		var status string
		switch _, _, err := toolchain.RecipeFor(name); {
		case available[name]:
			status = st.Good("ready")
		case err == nil:
			// Not an error: a runtime you have not installed on a worker that
			// does not need it is the ordinary case. It is a thing you can do
			// something about, and the doing is the part worth colouring.
			status = st.Muted("not installed -- ") +
				st.Cmd("je worker runtime install "+name)
		default:
			status = st.Muted("not installed -- no verified installer; install " +
				tc.Tool + " yourself")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, st.Muted(tc.Tool), status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(env.Stderr, "\n%s\n", env.ErrStyle.Muted(
		"A job declaring `language:` runs only on a worker that is ready for it.\n"+
			"Restart this worker after installing, so it advertises what it can do."))
	return nil
}

// runWorkerRuntimeInstall fetches a toolchain, verified.
func runWorkerRuntimeInstall(ctx context.Context, env *Env, language string) error {
	tc, recipe, err := toolchain.RecipeFor(language)
	if err != nil {
		return err
	}
	if _, err := toolchainLookPath(env)(tc.Tool); err == nil {
		fmt.Fprintf(env.Stdout, "%s is already available for %s\n", tc.Tool, language)
		return nil
	}

	archiveURL, checksumURL, err := recipe.URLs()
	if err != nil {
		return err
	}
	dir := filepath.Join(env.Layout.Toolchains(), tc.Tool)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "downloading %s\n", archiveURL)
	client := selfupdate.NewClient()
	name := filepath.Base(archiveURL)
	archive, sum, err := selfupdate.Download(ctx, client.HTTP, archiveURL, dir, name)
	if err != nil {
		return err
	}
	defer os.Remove(archive)

	want, err := publishedSum(ctx, env, client, checksumURL, name, recipe.List, dir)
	if err != nil {
		return err
	}
	if want != sum {
		// Stop dead, for the reason `je upgrade` stops dead: this is about to
		// put an executable on a machine and run somebody's code with it.
		return fmt.Errorf("checksum mismatch for %s\n  expected %s\n  got      %s",
			name, want, sum)
	}
	fmt.Fprintf(env.Stdout, "verified sha256 %s\n", sum[:16])

	if err := extractTar(archive, dir, recipe.Strip); err != nil {
		return err
	}
	for _, step := range recipe.Then {
		cmd := exec.CommandContext(ctx, filepath.Join(dir, step[0]), step[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w\n%s", strings.Join(step, " "), err, out)
		}
	}

	// One directory on PATH, whatever shape the archive had.
	if err := os.MkdirAll(env.Layout.ToolchainBin(), 0o755); err != nil {
		return err
	}
	if err := linkInto(env.Layout.ToolchainBin(), dir, recipe, tc); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "\n%s is ready for %s jobs on this worker.\n", tc.Tool, language)
	fmt.Fprintln(env.Stdout, "Restart the worker so it advertises the change:  je worker run")
	return nil
}

// publishedSum reads the checksum the publisher published for this archive.
func publishedSum(ctx context.Context, env *Env, client *selfupdate.Client,
	url, name string, list bool, dir string) (string, error) {

	path, _, err := selfupdate.Download(ctx, client.HTTP, url, dir, "checksums.txt")
	if err != nil {
		return "", fmt.Errorf("fetching the published checksum: %w", err)
	}
	defer os.Remove(path)

	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if list {
		sums, err := selfupdate.ParseChecksums(strings.NewReader(string(body)))
		if err != nil {
			return "", err
		}
		sum, ok := sums[name]
		if !ok {
			return "", fmt.Errorf("%s does not list %s", url, name)
		}
		return sum, nil
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("%s is empty", url)
	}
	return fields[0], nil
}

// linkInto puts the tool's entry point in one directory, so PATH needs one
// entry however the archive was laid out.
func linkInto(binDir, installDir string, recipe toolchain.Recipe, tc toolchain.Toolchain) error {
	// A recipe's Then step may have produced the tool itself (npm installing
	// pnpm), in which case it is already in the install directory's bin.
	candidates := []string{
		filepath.Join(installDir, "bin", tc.Tool),
		filepath.Join(installDir, recipe.Binary),
		filepath.Join(installDir, tc.Tool),
	}
	for _, target := range candidates {
		if _, err := os.Stat(target); err != nil {
			continue
		}
		link := filepath.Join(binDir, tc.Tool)
		_ = os.Remove(link)
		return os.Symlink(target, link)
	}
	return fmt.Errorf("installed, but %s is not where the recipe said it would be", tc.Tool)
}

// toolchainLookPath finds a tool, preferring what this worker installed.
//
// The engine's own toolchain directory comes first so that `je worker runtime
// install` has an effect without anybody editing a shell profile -- which would
// be the tool asking somebody else to finish its job.
func toolchainLookPath(env *Env) func(string) (string, error) {
	bin := env.Layout.ToolchainBin()
	return func(tool string) (string, error) {
		candidate := filepath.Join(bin, tool)
		if info, err := os.Stat(candidate); err == nil && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
		return exec.LookPath(tool)
	}
}

// extractTar unpacks a .tar.gz, dropping the leading path components the
// publisher wrapped it in.
//
// Its own function rather than selfupdate's ExtractBinary because that one
// pulls a single named file out; a toolchain is a tree -- Node is a whole
// prefix with bin/, lib/ and share/ -- and the parts have to keep their
// relationship to each other.
func extractTar(archivePath, dest string, strip int) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s is not a gzip archive: %w", filepath.Base(archivePath), err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := stripComponents(header.Name, strip)
		if name == "" {
			continue
		}
		// The same rule the source-tree extractor uses: an entry that escapes
		// the destination is refused rather than trusted, because a publisher
		// is not a more trusted source of archive entries than anybody else.
		target := filepath.Join(dest, name)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes the install directory", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
				os.FileMode(header.Mode)&0o777)
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
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		}
	}
}

func stripComponents(name string, n int) string {
	parts := strings.Split(filepath.ToSlash(name), "/")
	if len(parts) <= n {
		return ""
	}
	return filepath.Join(parts[n:]...)
}
