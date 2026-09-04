package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A CLI on the host of a containerised control plane can get the authority it
// needs without anybody touching docker.
//
// The gap this closes: the CLI installs the container, records where it is, and
// then cannot talk to it -- because the certificate it must verify against is
// inside a volume the host does not see. The instruction that produced was "run
// `je` inside the container", which is the tool failing rather than a step worth
// documenting.
//
// Uses the `worker` container name so it cannot collide with a control plane
// somebody is actually running here.
func TestTheAuthorityComesOutOfTheContainer(t *testing.T) {
	requireDocker(t)
	image := os.Getenv("JE_TEST_IMAGE")
	if image == "" {
		t.Skip("set JE_TEST_IMAGE to an image of this build to run this")
	}

	name := containerName("worker")
	run(t, "docker", "rm", "--force", name)
	t.Cleanup(func() { run(t, "docker", "rm", "--force", name) })

	// A control plane, in a container, with its data on a volume the host
	// cannot read -- which is the whole situation.
	if out, err := exec.Command("docker", "run", "--detach", "--name", name,
		"--publish", "127.0.0.1:0:7620",
		image, "control-plane", "run", "--addr", "0.0.0.0:7620").CombinedOutput(); err != nil {
		t.Fatalf("starting the control plane container: %s", out)
	}

	ctx := context.Background()
	if !containerExists(ctx, "worker") {
		t.Fatal("the container did not come up")
	}
	// It writes its authority on start; give it a moment to get there.
	dest := filepath.Join(t.TempDir(), "ca.crt")
	var err error
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if err = copyAuthorityFromContainer(ctx, "worker", dest); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("could not take the authority out of the container: %v", err)
	}

	body, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.HasPrefix(string(body), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("what came out is not a certificate:\n%s", truncate(string(body), 120))
	}

	// And the data directory is read from the image rather than assumed, so a
	// deployment that moved it is still found.
	if dir := containerDataDirNamed(ctx, name); dir != "/var/lib/je" {
		t.Errorf("data dir = %q, want the image's JE_DATA_DIR", dir)
	}

	// The other half: the enrollment token, so a worker on this host needs
	// nothing pasted. Same trust argument -- anybody who can copy this out of
	// the container could copy the CA key out instead.
	token, err := copyBootstrapTokenFrom(ctx, name)
	if err != nil {
		t.Fatalf("could not take the enrollment token out of the container: %v", err)
	}
	if token == "" || strings.ContainsAny(token, " \n\t") {
		t.Errorf("token = %q, want a single trimmed value", token)
	}
}

// A reset must not remove a deployment that belongs to a different data
// directory.
//
// The first run of `je reset` in a scratch directory listed the machine's real
// containers and volume for deletion, because a container is global to the
// machine and a data directory is not. That is the mistake worth a permanent
// test: it is silent, it looks like the command working, and what it costs is a
// database.
func TestResetLeavesAnotherDataDirectorysContainerAlone(t *testing.T) {
	requireDocker(t)

	name := containerName("worker")
	run(t, "docker", "rm", "--force", name)
	t.Cleanup(func() { run(t, "docker", "rm", "--force", name) })

	mine, theirs := t.TempDir(), t.TempDir()
	if out, err := exec.Command("docker", "create", "--name", name,
		"--label", ownerLabel+"="+theirs,
		"alpine", "true").CombinedOutput(); err != nil {
		t.Skipf("cannot create a fixture container: %s", out)
	}

	ctx := context.Background()
	if owned, observed := containerBelongsTo(ctx, name, mine); owned {
		t.Errorf("a container labelled for %s was claimed by %s (%s)", theirs, mine, observed)
	}
	if owned, _ := containerBelongsTo(ctx, name, theirs); !owned {
		t.Errorf("a container labelled for %s was not claimed by it", theirs)
	}

	// And an unlabelled container is claimed by the data directory whose files
	// it mounts, which is how deployments installed before the label are found.
	run(t, "docker", "rm", "--force", name)
	if out, err := exec.Command("docker", "create", "--name", name,
		"--volume", theirs+":/var/lib/je/jobs:ro",
		"alpine", "true").CombinedOutput(); err != nil {
		t.Skipf("cannot create a fixture container: %s", out)
	}
	if owned, _ := containerBelongsTo(ctx, name, theirs); !owned {
		t.Errorf("a container mounting %s was not claimed by it", theirs)
	}
	if owned, _ := containerBelongsTo(ctx, name, mine); owned {
		t.Errorf("a container mounting %s was claimed by %s", theirs, mine)
	}
}
