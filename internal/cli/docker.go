package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Running a component as a container, as an alternative to a native service.
//
// Both are "keep this alive without me"; they differ in what supervises it.
// The CLI generates the invocation either way, which is the whole point: the
// deployment is reproducible because the tool that knows what a working one
// looks like is the one that wrote it down.
//
// Docker is driven through its CLI rather than its API. The API would mean a
// large dependency and a socket path to get wrong on every platform, to gain
// nothing a person cannot already do by hand -- and being able to do it by hand
// is what makes `--print` below worth having.

// imageRepo is where release.yml publishes to.
const imageRepo = "ghcr.io/jdmorlan/job-engine"

// containerName is what each component's container is called, so `docker ps`
// reads clearly and `remove` can find it again.
func containerName(component string) string { return "je-" + component }

// dockerAvailable reports whether we can talk to a daemon.
//
// Both halves matter: the binary may be installed while Docker Desktop is not
// running, and "docker: command not found" and "cannot connect to the Docker
// daemon" want different advice.
func dockerAvailable() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not installed on this machine")
	}
	out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker is installed but not running: %s",
			strings.TrimSpace(lastLine(string(out))))
	}
	return nil
}

// imageRef pins the image to this binary's own version.
//
// Pinned rather than :latest, because a control plane and the CLI talking to it
// must agree on a version (C10 refuses skew), and :latest would silently move
// underneath a machine that has been up for a month.
func imageRef(version string) string {
	if version == "" || version == "dev" {
		// A dev build has no published image. Saying so beats resolving to
		// :latest and running a different version than the one being debugged.
		return ""
	}
	return imageRepo + ":" + version
}

// printableImage is imageRef for --print, which must render the shape of the
// command even on a build whose image was never published.
//
// Refusing to print because the image does not exist would defeat the point:
// --print exists to be readable before anything happens, including on a
// development build where nothing can happen.
func printableImage(version string) string {
	if ref := imageRef(version); ref != "" {
		return ref
	}
	return imageRepo + ":<version>"
}

// dockerRun builds the argv for one component's container.
type dockerSpec struct {
	component string
	image     string
	args      []string // the component's own flags, after its subcommand
	ports     []string
	volumes   []string
	env       []string
	network   string
}

func (d dockerSpec) argv() []string {
	argv := []string{
		"run", "--detach",
		"--name", containerName(d.component),
		// Restart policy is the whole reason to use a container rather than
		// `je <component> run` in a terminal: it is what survives a reboot.
		// `unless-stopped` rather than `always` so that a deliberate
		// `docker stop` stays stopped.
		"--restart", "unless-stopped",
	}
	for _, p := range d.ports {
		argv = append(argv, "--publish", p)
	}
	for _, v := range d.volumes {
		argv = append(argv, "--volume", v)
	}
	for _, e := range d.env {
		argv = append(argv, "--env", e)
	}
	if d.network != "" {
		argv = append(argv, "--network", d.network)
	}
	argv = append(argv, d.image, d.component, "run")
	return append(argv, d.args...)
}

// String renders the command as a person would type it, for --print and for
// telling somebody what was just done on their behalf.
func (d dockerSpec) String() string {
	return "docker " + strings.Join(d.argv(), " ")
}

// start runs the container, replacing any existing one with the same name.
func (d dockerSpec) start(ctx context.Context) error {
	// Removed first so that `install` is repeatable. Leaving a stale container
	// and failing with "name already in use" would make re-running the setup
	// command -- the obvious thing to do after changing a flag -- an error.
	_ = exec.CommandContext(ctx, "docker", "rm", "--force", containerName(d.component)).Run()

	out, err := exec.CommandContext(ctx, "docker", d.argv()...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting the %s container: %s",
			d.component, strings.TrimSpace(lastLine(string(out))))
	}
	return nil
}

// stopContainer removes a component's container if it exists.
func stopContainer(ctx context.Context, component string) (bool, error) {
	name := containerName(component)
	if err := exec.CommandContext(ctx, "docker", "inspect", name).Run(); err != nil {
		return false, nil // not there
	}
	out, err := exec.CommandContext(ctx, "docker", "rm", "--force", name).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("removing %s: %s", name, strings.TrimSpace(lastLine(string(out))))
	}
	return true, nil
}

// lastLine is the useful part of a docker error, which is usually verbose.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) == 0 {
		return s
	}
	return lines[len(lines)-1]
}

// networkName is the bridge both components share when both are containers.
const networkName = "je"

// ensureNetwork creates the shared bridge if it is not there.
func ensureNetwork(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "docker", "network", "inspect", networkName).Run(); err == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "network", "create", networkName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("creating the %s network: %s",
			networkName, strings.TrimSpace(lastLine(string(out))))
	}
	return nil
}

// containerExists reports whether a component is running as a container here.
func containerExists(ctx context.Context, component string) bool {
	return exec.CommandContext(ctx, "docker", "inspect", containerName(component)).Run() == nil
}

// workerTarget is the address a containerised worker should dial.
//
// This is the bug a worker in a container hits immediately and silently: the
// control plane's address on the host is 127.0.0.1, and inside a container
// 127.0.0.1 is the container itself. It dials nothing, forever, and looks like
// a worker that simply never registers.
//
// Two cases, and they need different answers. If the control plane is also a
// container, both join a user-defined bridge and it is reachable by container
// name -- which is what compose.yaml does, for the same reason. If it is on the
// host, Docker Desktop publishes the host as host.docker.internal.
func workerTarget(ctx context.Context, hostAddr string) (target, network string) {
	_, port := splitHostPort(hostAddr)

	if containerExists(ctx, "control-plane") {
		return containerName("control-plane") + ":7620", networkName
	}
	return "host.docker.internal:" + port, ""
}

// splitHostPort is a forgiving split that tolerates a bare port or host.
func splitHostPort(addr string) (host, port string) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "7620"
	}
	return addr[:i], addr[i+1:]
}
