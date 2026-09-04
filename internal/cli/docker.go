package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
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

	// cmd is the whole trailing command, when it is known exactly rather than
	// assembled. Set only by inspectContainer, which reads what a container is
	// actually running: an upgrade must reproduce the command that is there,
	// including flags this version of the CLI would not have written.
	cmd []string
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
	argv = append(argv, d.image)
	if len(d.cmd) > 0 {
		return append(argv, d.cmd...)
	}
	argv = append(argv, d.component, "run")
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

// containerImageTag reports the tag a component's container is running.
//
// The tag is how a container says which version it is: there is no runtime file
// to ask, and asking the process would mean the container being healthy enough
// to answer -- which is exactly what may not be true when somebody is trying to
// work out why nothing works.
func containerImageTag(ctx context.Context, component string) string {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.Config.Image}}", containerName(component)).Output()
	if err != nil {
		return ""
	}
	image := strings.TrimSpace(string(out))
	if i := strings.LastIndex(image, ":"); i >= 0 {
		return image[i+1:]
	}
	return ""
}

// inspectContainer reconstructs the spec of a running container, so it can be
// recreated on a new image without anybody having to remember how it was set up.
//
// Reading the container rather than a file this CLI wrote is what makes upgrade
// work for a deployment installed by an older version -- which is every
// deployment that exists at the moment this is added. It also means the flags
// reproduced are the ones actually in use rather than the ones this version
// would choose.
//
// Two things docker reports are not what you would pass back:
//
//   - Docker Desktop rewrites a bind source to /host_mnt/<path>. Passing that
//     back creates a bind to a path the host does not have, so the jobs
//     directory would quietly become an empty one inside the VM.
//   - Config.Env includes the image's own defaults. Passing them back is
//     harmless but it pins them: a later image that changed a default would be
//     overridden by the value baked into the image being replaced.
func inspectContainer(ctx context.Context, component string) (dockerSpec, error) {
	name := containerName(component)
	raw, err := exec.CommandContext(ctx, "docker", "inspect", name).Output()
	if err != nil {
		return dockerSpec{}, fmt.Errorf("inspecting %s: %w", name, err)
	}

	var inspected []struct {
		Config struct {
			Image string   `json:"Image"`
			Cmd   []string `json:"Cmd"`
			Env   []string `json:"Env"`
		} `json:"Config"`
		HostConfig struct {
			Binds        []string                       `json:"Binds"`
			NetworkMode  string                         `json:"NetworkMode"`
			PortBindings map[string][]dockerPortBinding `json:"PortBindings"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(raw, &inspected); err != nil {
		return dockerSpec{}, fmt.Errorf("reading docker inspect for %s: %w", name, err)
	}
	if len(inspected) == 0 {
		return dockerSpec{}, fmt.Errorf("docker knows no container called %s", name)
	}
	c := inspected[0]

	spec := dockerSpec{
		component: component,
		image:     c.Config.Image,
		cmd:       c.Config.Cmd,
		env:       withoutImageDefaults(ctx, c.Config.Image, c.Config.Env),
	}
	if c.HostConfig.NetworkMode != "" && c.HostConfig.NetworkMode != "default" {
		spec.network = c.HostConfig.NetworkMode
	}
	for _, bind := range c.HostConfig.Binds {
		spec.volumes = append(spec.volumes, hostBind(bind))
	}
	// Sorted, so a recreated container's command is stable rather than
	// depending on Go's map iteration -- which matters because it is printed.
	for _, port := range sortedKeys(c.HostConfig.PortBindings) {
		for _, b := range c.HostConfig.PortBindings[port] {
			container := strings.TrimSuffix(port, "/tcp")
			published := b.HostPort + ":" + container
			if b.HostIP != "" {
				published = b.HostIP + ":" + published
			}
			spec.ports = append(spec.ports, published)
		}
	}
	return spec, nil
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// hostBind undoes Docker Desktop's rewriting of a bind source.
//
// On macOS the host path /Users/x/.je/jobs is reported as
// /host_mnt/Users/x/.je/jobs, which is how the Linux VM sees it and not a path
// this machine has. Passing it back would mount an empty directory and the
// symptom would be a control plane that suddenly loads no jobs.
func hostBind(bind string) string {
	const desktopPrefix = "/host_mnt/"
	if strings.HasPrefix(bind, desktopPrefix) {
		return "/" + strings.TrimPrefix(bind, desktopPrefix)
	}
	return bind
}

// withoutImageDefaults keeps only the environment the container was given
// explicitly, so replacing it does not pin the old image's defaults.
func withoutImageDefaults(ctx context.Context, image string, env []string) []string {
	defaults := map[string]bool{}
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{json .Config.Env}}", image).Output()
	if err == nil {
		var baked []string
		if json.Unmarshal(out, &baked) == nil {
			for _, e := range baked {
				defaults[e] = true
			}
		}
	}

	var kept []string
	for _, e := range env {
		if defaults[e] {
			continue
		}
		// A variable the image also sets, to a different value, is one somebody
		// chose -- TZ is exactly this -- so it is kept.
		kept = append(kept, e)
	}
	return kept
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pullImage fetches a tag before anything is torn down, so a network failure
// costs nothing rather than leaving the component stopped.
//
// An image already on this machine is enough. `docker run` would use it without
// asking the registry, so refusing here would fail an upgrade that is going to
// work -- on a host with no route to ghcr, or one where the image was loaded by
// hand. The pull is still attempted first, because the usual case is that it is
// not here yet.
func pullImage(ctx context.Context, image string) (pulled bool, err error) {
	out, pullErr := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput()
	if pullErr == nil {
		return true, nil
	}
	if imagePresent(ctx, image) {
		return false, nil
	}
	return false, fmt.Errorf("pulling %s: %s", image, strings.TrimSpace(lastLine(string(out))))
}

// imagePresent reports whether docker already has this exact tag locally.
func imagePresent(ctx context.Context, image string) bool {
	return exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() == nil
}

// retag replaces the version in an image reference, keeping the repository.
func retag(image, version string) string {
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i+1:], "/") {
		return image[:i] + ":" + version
	}
	return image + ":" + version
}
