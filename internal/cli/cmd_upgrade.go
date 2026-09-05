package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/selfupdate"
	"github.com/jdmorlan/job-engine/internal/service"
)

func init() {
	register(&Command{
		Name:  "upgrade",
		Usage: "upgrade this deployment: the binary, and what is running here",
		Long: "Upgrades this deployment: the binary first, verified against the\n" +
			"checksum published with the release, then whatever this machine runs.\n\n" +
			"`je` is one binary playing three parts, so replacing it upgrades the CLI,\n" +
			"the control plane and any worker here at once -- but a running process\n" +
			"keeps executing the old code until it is restarted. This offers to do\n" +
			"that, for a native service and for a container alike. You should not\n" +
			"have to know which one you have.\n\n" +
			"It asks first, because restarting a control plane drops whatever is\n" +
			"mid-flight and that is a poor thing to do as a side effect of swapping a\n" +
			"file. --yes skips the question; --restart skips the download and just\n" +
			"restarts what is behind.\n\n" +
			"The exception is a deployment you drive with `docker compose`: that\n" +
			"compose file is yours and owns its containers, so this reports them and\n" +
			"changes nothing.\n\n" +
			"--from installs a binary you built instead of a published release, which\n" +
			"is how you try a change on your own deployment without tagging one:\n\n" +
			"  make build && je upgrade --from ./je\n\n" +
			"Nothing verifies that file, because there is nothing to verify it\n" +
			"against -- you built it. Everything after that is the same upgrade.",
		Local: true,
		Run:   runUpgrade,
	})
}

func runUpgrade(ctx context.Context, env *Env, args []string) error {
	cmd := commands["upgrade"]
	fs := newFlagSet(cmd, env)
	check := fs.Bool("check", false, "report what is available without installing it")
	yes := fs.Bool("yes", false, "restart the components on this machine without asking")
	restartOnly := fs.Bool("restart", false, "skip the download; just restart what is on the old version")
	from := fs.String("from", "", "install this binary instead of downloading a release")
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

	// --restart is the other half of answering "no" to the prompt below, and
	// of an upgrade that replaced the binary and then failed partway. It needs
	// no network: what is on the old version is decided by comparing against
	// the binary you are running, which is already the new one.
	if *restartOnly {
		upgradeDeployment(ctx, env, env.Version, *yes)
		return nil
	}

	if *from != "" {
		return upgradeFromFile(ctx, env, *from, *yes)
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

	upgradeDeployment(ctx, env, release.TagName, *yes)
	return nil
}

// upgradeFromFile installs a binary you built instead of one we published.
//
// The loop it exists for is the obvious one: change something, run it on your
// own deployment, see whether it was right. Going through a tag, a workflow and
// a download to answer that costs ten minutes, and a ten minute loop is not a
// loop -- so people stop using their own deployment to test with, which is the
// worst outcome available.
//
// Everything after the source of the file is identical to a released upgrade:
// the same refusal to write somewhere managed by a package manager, the same
// atomic replace, and the same restart of whatever this machine runs (D26). The
// only thing that changes is where the bytes came from.
//
// What it deliberately does NOT do is verify a checksum. There is nothing to
// verify against: you built this. Saying so is better than inventing a
// ceremony -- a hash of the file you just made, checked against itself, would
// look like a guarantee and be none.
func upgradeFromFile(ctx context.Context, env *Env, path string, yes bool) error {
	source, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	target, err := selfupdate.CurrentBinary()
	if err != nil {
		return err
	}
	if source == target {
		return fmt.Errorf("%s is already the binary you are running", source)
	}
	if owner := selfupdate.ManagedElsewhere(target); owner != "" {
		return fmt.Errorf("this binary is managed by %s, so upgrading it here would be undone", owner)
	}
	if !selfupdate.Writable(target) {
		return fmt.Errorf("cannot write to %s", filepath.Dir(target))
	}

	// Run it before installing it. The release workflow smoke-tests the
	// artifact it is about to publish rather than trusting that it would have
	// worked, and a binary you built on the machine you are about to install
	// it on deserves at least as much: a build for the wrong architecture, or
	// one that panics on startup, should not become your `je`.
	version, err := binaryVersion(ctx, source)
	if err != nil {
		return err
	}

	// Copied rather than renamed, and into the target's own directory because
	// the last step is a rename and rename cannot cross filesystems. Renaming
	// the source would move the binary out of the tree you built it in, which
	// is somebody's `make build` output and not ours to consume.
	staged := filepath.Join(filepath.Dir(target), ".je-upgrade-staged")
	if err := copyFile(source, staged); err != nil {
		return err
	}
	defer os.Remove(staged)

	if err := selfupdate.Replace(target, staged); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "installed %s at %s\n", version, target)
	fmt.Fprintf(env.Stderr, "je: from %s, which nothing verified -- you built it\n", source)

	upgradeDeployment(ctx, env, version, yes)
	return nil
}

// binaryVersion asks a binary what it is, which doubles as the check that it
// runs at all on this machine.
func binaryVersion(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("%s does not run on this machine: %w", path, err)
	}
	// "je v0.9.0 darwin/arm64" -- the version is the second field, and a
	// locally built one says "dev", which is exactly what should then show up
	// in `je workers` and be compared by C10.
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", fmt.Errorf("%s did not report a version", path)
	}
	return fields[1], nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// upgradeDeployment brings the components on this machine to the new version.
//
// `je upgrade` used to replace the binary and then *tell* you what else to run.
// That was defensible for a native service and indefensible for a container: it
// printed a docker command, which is the one thing this CLI exists to spare
// somebody. A person running a job engine should not have to know it is a
// container any more than they have to know it is SQLite.
//
// It still asks first. Replacing a running control plane drops whatever is
// mid-flight, and doing that as a silent side effect of swapping a file is a
// worse default than one extra keystroke. Non-interactive means no: a script
// that wants the whole thing says --yes.
func upgradeDeployment(ctx context.Context, env *Env, newVersion string, yes bool) {
	stale := staleComponents(ctx, env, newVersion)
	if len(stale) == 0 {
		return
	}

	if projects := composeProjects(ctx); len(projects) > 0 {
		// Named rather than silently skipped. These containers run this image
		// and this command is not going to touch them, and the person deserves
		// to hear that from the tool rather than work it out from a version
		// that did not change.
		fmt.Fprintf(env.Stderr,
			"\nnote: %s also runs job-engine under docker compose (%s).\n"+
				"      That compose file is yours and owns those containers, so they are\n"+
				"      left alone:  docker compose pull && docker compose up -d\n",
			"this machine", strings.Join(projects, ", "))
	}

	fmt.Fprintf(env.Stdout, "\nstill running the old version:\n")
	for _, c := range stale {
		fmt.Fprintf(env.Stdout, "  %-14s %-8s (%s)\n", c.component, c.version, c.how)
	}
	// C10 raises the stakes: a worker refuses to register against a control
	// plane on another version, so a half-upgraded deployment does not drift,
	// it stops.
	fmt.Fprintln(env.Stdout,
		"\nA worker refuses to attach across a version gap (D20/C10), so a\n"+
			"half-upgraded deployment stops rather than drifts.")

	if !yes {
		if !interactive(env) {
			// Nothing was asked, so nothing should be implied. A cron job or a
			// CI step running this must not have its control plane restarted
			// because a prompt defaulted to yes somewhere out of sight.
			fmt.Fprintln(env.Stdout,
				"\nleft alone: there is no terminal here to ask.\n"+
					"  je upgrade --restart --yes")
			return
		}
		if !confirm(env, "\nrestart them on "+newVersion+"?  [y/N] ") {
			fmt.Fprintln(env.Stdout, "\nleft alone. When you are ready:  je upgrade --restart")
			return
		}
	}

	for _, c := range stale {
		if c.ours() {
			fmt.Fprintf(env.Stdout, "\n%s: ", c.component)
		} else {
			fmt.Fprintf(env.Stdout, "\n%s:\n", c.component)
		}
		if err := c.upgrade(ctx, env, newVersion); err != nil {
			// One failure must not stop the others: a control plane that came
			// up and a worker that did not is a state somebody can act on,
			// and it is better than stopping at the first problem.
			fmt.Fprintf(env.Stderr, "could not upgrade: %v\n", err)
			continue
		}
		if c.ours() {
			fmt.Fprintf(env.Stdout, "restarted on %s\n", newVersion)
		}
	}
}

// staleComponent is one thing on this machine running the wrong version, and
// what to do about it.
type staleComponent struct {
	component string
	version   string
	how       string // how it is supervised, for the human
	upgrade   func(ctx context.Context, env *Env, version string) error
}

// ours reports whether this command can restart the component itself.
//
// The one that is not ours is a control plane somebody started in a terminal:
// it is described by the runtime file like a service, and it is theirs to stop.
// The distinction is only about rendering -- claiming "restarted on v0.9.0"
// after printing "it is yours to restart" would be the tool contradicting
// itself in consecutive lines.
func (c staleComponent) ours() bool { return c.how != inATerminal }

// inATerminal marks the deployment shape somebody is standing in.
const inATerminal = "in a terminal"

// staleComponents finds what this machine runs and what version each is on.
//
// Containers first, because their version is visible without the process being
// healthy enough to answer -- which is exactly what may not be true when
// somebody is upgrading to fix something.
func staleComponents(ctx context.Context, env *Env, newVersion string) []staleComponent {
	var out []staleComponent

	for _, component := range []string{"control-plane", "worker", "web"} {
		if !containerExists(ctx, component) {
			continue
		}
		tag := containerImageTag(ctx, component)
		if tag == "" || sameVersion(tag, newVersion) {
			continue
		}
		out = append(out, staleComponent{
			component: component,
			version:   tag,
			how:       "container",
			upgrade:   upgradeContainer(component),
		})
	}

	// A native control plane, which the runtime file describes. Only when
	// there is no container for it: both at once is not a deployment shape,
	// and the container is the one that owns the port.
	if !containerExists(ctx, "control-plane") {
		if info, err := daemon.ReadRuntime(env.Layout.Runtime()); err == nil &&
			!sameVersion(info.Version, newVersion) {
			// A runtime file says a control plane is running. It does not say
			// how it was started, and the two cases want opposite things: a
			// service is ours to restart, and one somebody launched in their
			// terminal is theirs. Offering to restart the second produced
			// `launchctl kickstart: Could not find service ... in domain for
			// user gui: 501`, which is a true sentence about the wrong
			// question.
			how, upgrade := "native service", restartNativeService(service.ControlPlane)
			if !serviceInstalled(service.ControlPlane) {
				how, upgrade = inATerminal, restartYourself(info.PID)
			}
			out = append(out, staleComponent{
				component: "control-plane",
				version:   info.Version,
				how:       how,
				upgrade:   upgrade,
			})
		}
	}
	return out
}

// upgradeContainer recreates a component's container on the new image.
//
// The spec comes from the container itself, so the flags, mounts and published
// ports it comes back with are the ones it had -- including any this version of
// the CLI would not have written. Pulled before anything is torn down, so a
// network failure costs nothing and leaves the old container running.
func upgradeContainer(component string) func(context.Context, *Env, string) error {
	return func(ctx context.Context, env *Env, version string) error {
		spec, err := inspectContainer(ctx, component)
		if err != nil {
			return err
		}
		spec.image = retag(spec.image, version)

		fmt.Fprintf(env.Stdout, "pulling %s\n", spec.image)
		pulled, err := pullImage(ctx, spec.image)
		if err != nil {
			return err
		}
		if !pulled {
			fmt.Fprintf(env.Stdout, "  (registry unreachable; using the copy already here)\n")
		}
		// start force-removes the existing container first, so this is the
		// replacement rather than a second one fighting for the port.
		fmt.Fprintf(env.Stdout, "  ")
		return spec.start(ctx)
	}
}

// restartNativeService restarts a launchd or systemd unit, which picks up the
// binary that was just replaced.
// serviceInstalled reports whether this component has a unit file, which is
// what makes it something this command can restart.
func serviceInstalled(c service.Component) bool {
	manager, err := service.New(c)
	if err != nil {
		return false
	}
	state, err := manager.Status()
	if err != nil {
		return false
	}
	return state.Installed
}

// restartYourself is what to do about a control plane running in somebody's
// terminal, which is: tell them, because only they can.
//
// Not an error. It is the ordinary shape while developing -- `je quickstart` in
// one window, `make install` in another -- and a command that failed there
// would be failing at the thing working correctly.
func restartYourself(pid int) func(context.Context, *Env, string) error {
	return func(_ context.Context, env *Env, _ string) error {
		fmt.Fprintf(env.Stdout,
			"  the control plane is running in a terminal (pid %d), so it is yours to\n"+
				"  restart: stop it there and start it again to pick this up\n", pid)
		return nil
	}
}

func restartNativeService(c service.Component) func(context.Context, *Env, string) error {
	return func(ctx context.Context, env *Env, version string) error {
		manager, err := service.New(c)
		if err != nil {
			return err
		}
		return manager.Restart()
	}
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

// composeProjects names any docker compose projects running this image.
//
// The one deployment shape this command deliberately does not manage: a compose
// file is a thing somebody wrote and version-controlled, and recreating its
// containers from underneath it would leave compose's idea of the world and the
// world disagreeing. Saying so is the whole contribution -- the alternative is a
// person watching `je upgrade` succeed and the version not change.
func composeProjects(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "docker", "ps",
		"--filter", "label=com.docker.compose.project",
		"--format", "{{.Label \"com.docker.compose.project\"}}\t{{.Image}}").Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var projects []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		project, image, ok := strings.Cut(line, "\t")
		if !ok || project == "" || !strings.Contains(image, "job-engine") {
			continue
		}
		if !seen[project] {
			seen[project] = true
			projects = append(projects, project)
		}
	}
	sort.Strings(projects)
	return projects
}
