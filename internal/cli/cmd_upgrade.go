package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/selfupdate"
)

func init() {
	register(&Command{
		Name:  "upgrade",
		Usage: "download and install the latest release",
		Long: "Replaces this binary in place, after verifying its SHA-256 against the\n" +
			"checksum file published with the release.\n\n" +
			"A running control plane keeps executing the old version until it is\n" +
			"restarted; `je status` says so rather than leaving you to wonder why\n" +
			"nothing changed. Where the control plane is a container, upgrading it is\n" +
			"`docker compose pull && docker compose up -d` rather than this command --\n" +
			"this upgrades the CLI you are typing at.",
		Run: runUpgrade,
	})
}

func runUpgrade(ctx context.Context, env *Env, args []string) error {
	cmd := commands["upgrade"]
	fs := newFlagSet(cmd, env)
	check := fs.Bool("check", false, "report what is available without installing it")
	repo := fs.String("repo", "", "GitHub repository to fetch releases from")
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	// NewClient already honours JE_RELEASE_REPO and JE_RELEASE_API; the flag
	// only overrides when it was actually given, so passing nothing does not
	// silently reset an environment override back to the default.
	client := selfupdate.NewClient()
	if *repo != "" {
		client.Repo = *repo
	}

	release, err := client.Latest(ctx)
	if err != nil {
		return err
	}

	current := env.Version
	fmt.Fprintf(env.Stdout, "  current  %s\n  latest   %s\n\n", current, release.TagName)

	if sameVersion(current, release.TagName) {
		fmt.Fprintln(env.Stdout, "already up to date")
		return nil
	}
	if *check {
		fmt.Fprintf(env.Stdout, "run `je upgrade` to install %s\n", release.TagName)
		return nil
	}

	target, err := selfupdate.CurrentBinary()
	if err != nil {
		return err
	}
	// Refuse before downloading anything, so a doomed upgrade costs nothing
	// and says why immediately.
	if owner := selfupdate.ManagedElsewhere(target); owner != "" {
		return fmt.Errorf("this binary is managed by %s, so upgrading it here would be undone", owner)
	}
	if !selfupdate.Writable(target) {
		return fmt.Errorf("cannot write to %s\n\nEither re-run with sudo, or install somewhere you own:\n"+
			"  JE_INSTALL_DIR=~/.local/bin curl -fsSL https://raw.githubusercontent.com/%s/main/install.sh | sh",
			filepath.Dir(target), client.Repo)
	}

	goos, goarch := selfupdate.Platform()
	asset, err := release.AssetFor(goos, goarch)
	if err != nil {
		return err
	}
	sumsAsset, err := release.Checksums()
	if err != nil {
		return err
	}

	// Everything lands in the target's own directory, because the final step
	// is a rename and rename cannot cross filesystems.
	dir := filepath.Dir(target)

	fmt.Fprintf(env.Stdout, "downloading %s (%s)\n", asset.Name, humanBytes(asset.Size))
	archive, gotSum, err := selfupdate.Download(ctx, client.HTTP, asset.URL, dir, asset.Name)
	if err != nil {
		return err
	}
	defer os.Remove(archive)

	sumsPath, _, err := selfupdate.Download(ctx, client.HTTP, sumsAsset.URL, dir, sumsAsset.Name)
	if err != nil {
		return err
	}
	defer os.Remove(sumsPath)

	sumsBody, err := os.ReadFile(sumsPath)
	if err != nil {
		return err
	}
	sums, err := selfupdate.ParseChecksums(bytes.NewReader(sumsBody))
	if err != nil {
		return err
	}

	want, ok := sums[asset.Name]
	if !ok {
		return fmt.Errorf("%s does not list %s, so the download cannot be verified",
			selfupdate.ChecksumsName, asset.Name)
	}
	if want != gotSum {
		// Stop dead. This downloads an executable and is about to run it as
		// you; a mismatch is never worth continuing through.
		return fmt.Errorf("checksum mismatch for %s\n  expected %s\n  got      %s",
			asset.Name, want, gotSum)
	}
	fmt.Fprintf(env.Stdout, "verified sha256 %s\n", gotSum[:16])

	extracted, err := selfupdate.ExtractBinary(archive, "je", dir)
	if err != nil {
		return err
	}
	defer os.Remove(extracted)

	if err := selfupdate.Replace(target, extracted); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "\nupgraded to %s at %s\n", release.TagName, target)

	warnStaleControlPlane(env, release.TagName)
	return nil
}

// warnStaleControlPlane points out that a running control plane is still the
// old binary.
//
// The file on disk changed; the process did not. Without this the next thing
// that happens is somebody upgrading, seeing no change in behaviour, and
// concluding the upgrade did not work.
func warnStaleControlPlane(env *Env, newVersion string) {
	info, err := daemon.ReadRuntime(env.Layout.Runtime())
	if err != nil {
		return
	}
	if sameVersion(info.Version, newVersion) {
		return
	}
	fmt.Fprintf(env.Stderr,
		"\nnote: a control plane is still running %s (pid %d).\n"+
			"      Restart it to pick up %s.\n",
		info.Version, info.PID, newVersion)
}

// sameVersion compares versions ignoring a leading v, so v0.2.0 and 0.2.0 are
// the same release rather than an upgrade loop.
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
