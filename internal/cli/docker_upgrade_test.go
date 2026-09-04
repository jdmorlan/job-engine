package cli

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// Upgrading a container must reproduce everything about it except the tag.
//
// This is the property the whole feature rests on: `je upgrade` recreates a
// container from what docker reports, so anything it fails to carry across is
// silently lost -- a published port, a mount, a flag somebody passed. Reading
// the reconstruction is not enough to know that, because the shapes docker
// reports are not the shapes you pass back (a bind source comes back rewritten
// on Docker Desktop, and the environment includes the image's own defaults).
//
// Uses the `worker` component name so it cannot collide with a control plane or
// web container somebody is actually running on this machine.
func TestUpgradingAContainerKeepsEverythingButTheTag(t *testing.T) {
	requireDocker(t)

	const (
		old = "ghcr.io/jdmorlan/job-engine:upgrade-test-old"
		new = "ghcr.io/jdmorlan/job-engine:upgrade-test-new"
	)
	// Any image that stays up; this test never talks to the process, only to
	// docker about it.
	buildTestImage(t, old)
	buildTestImage(t, new)

	name := containerName("worker")
	run(t, "docker", "rm", "--force", name)
	t.Cleanup(func() { run(t, "docker", "rm", "--force", name) })

	dir := t.TempDir()
	if out, err := exec.Command("docker", "run", "--detach", "--name", name,
		"--restart", "unless-stopped",
		"--publish", "127.0.0.1:7699:7699",
		"--volume", dir+":/mnt/jobs:ro",
		"--env", "TZ=America/Chicago",
		old, "sleep", "600").CombinedOutput(); err != nil {
		t.Fatalf("starting the fixture container: %s", out)
	}

	spec, err := inspectContainer(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}

	// The command, the mount, the published port and the chosen environment all
	// survive; the image's own defaults do not come back as overrides.
	if got := strings.Join(spec.cmd, " "); got != "sleep 600" {
		t.Errorf("cmd = %q, want %q", got, "sleep 600")
	}
	if len(spec.ports) != 1 || spec.ports[0] != "127.0.0.1:7699:7699" {
		t.Errorf("ports = %v", spec.ports)
	}
	if len(spec.volumes) != 1 || !strings.HasSuffix(spec.volumes[0], ":/mnt/jobs:ro") {
		t.Errorf("volumes = %v", spec.volumes)
	}
	if !strings.HasPrefix(spec.volumes[0], dir) {
		// The Docker Desktop rewriting: a source of /host_mnt/<path> would
		// mount a directory the host does not have, and the symptom is a
		// component that comes back up seeing nothing.
		t.Errorf("bind source = %q, want it to start with the host path %q",
			spec.volumes[0], dir)
	}
	for _, e := range spec.env {
		if strings.HasPrefix(e, "PATH=") {
			t.Errorf("env carries the image's own default %q", e)
		}
	}
	if !contains(spec.env, "TZ=America/Chicago") {
		t.Errorf("env = %v, want the TZ that was chosen", spec.env)
	}

	// And the recreate actually lands on the new image.
	spec.image = retag(spec.image, "upgrade-test-new")
	if spec.image != new {
		t.Fatalf("retag = %q, want %q", spec.image, new)
	}
	if err := spec.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := imageOf(t, name); got != new {
		t.Errorf("after upgrade the container runs %q, want %q", got, new)
	}
	// Same shape as before, on the new image.
	after, err := inspectContainer(context.Background(), "worker")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(after.cmd, " ") != "sleep 600" ||
		len(after.ports) != 1 || after.ports[0] != "127.0.0.1:7699:7699" {
		t.Errorf("the recreated container differs: cmd=%v ports=%v", after.cmd, after.ports)
	}
}

func requireDocker(t *testing.T) {
	t.Helper()
	if err := dockerAvailable(); err != nil {
		t.Skip("docker is not available: ", err)
	}
}

// buildTestImage makes a tiny image under the tag, so the test never depends on
// a registry or on which versions happen to be on this machine.
func buildTestImage(t *testing.T, tag string) {
	t.Helper()
	cmd := exec.Command("docker", "build", "--quiet", "--tag", tag, "-")
	cmd.Stdin = strings.NewReader("FROM busybox\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build a test image (%v): %s", err, out)
	}
	t.Cleanup(func() { run(t, "docker", "rmi", "--force", tag) })
}

func imageOf(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "--format", "{{.Config.Image}}", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	_ = exec.Command(name, args...).Run()
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
