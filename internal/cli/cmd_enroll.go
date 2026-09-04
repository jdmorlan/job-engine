package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"slices"
	"strings"

	"crypto/tls"
	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/ca"
	"github.com/jdmorlan/job-engine/internal/store"
	"io"
	"net/http"
	"time"
)

func init() {
	register(&Command{
		Name:  "enroll",
		Args:  "<name> [--labels a,b] [--client]",
		Usage: "issue a one-time token that lets one machine hold an identity",
		Long: "Run this where the control plane is, then redeem the token on the\n" +
			"machine that is becoming a worker:\n\n" +
			"  je enroll macbook --labels macos    (here)\n" +
			"  je worker run --token <token>       (there)\n\n" +
			"With --client it enrolls a person at a terminal instead, redeemed with\n" +
			"`je identity join`. A machine that is both keeps one certificate\n" +
			"carrying both roles.\n\n" +
			"The FIRST client identity changes the deployment: from then on every\n" +
			"request that changes something must present a certificate. This command\n" +
			"says so before the token is redeemed.\n\n" +
			"The name and labels are decided HERE, not by the machine redeeming\n" +
			"the token. That is the point: a label is a capability, and a worker\n" +
			"that advertises its own can grant itself whatever a label gates.\n\n" +
			"The token is single-use and short-lived. It is a bearer credential --\n" +
			"whoever holds it becomes this worker -- so it is shown once and the\n" +
			"control plane keeps only a hash.",
		Run: runEnroll,
	})
}

func runEnroll(ctx context.Context, env *Env, args []string) error {
	cmd := commands["enroll"]
	fs := newFlagSet(cmd, env)
	labels := fs.String("labels", "", "comma-separated capabilities this worker may advertise")
	client := fs.Bool("client", false, "enroll a person at a terminal rather than a worker")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: je enroll <name> [--labels a,b] [--client]")
	}

	// Both is a real thing and worth allowing: a laptop that runs `macos` jobs
	// and is also where somebody types `je run` is one machine with one
	// certificate, and making it enroll twice would give it two identities and
	// make "who did this" depend on which command was running.
	var roles []string
	if *client {
		roles = append(roles, store.RoleClient)
	}
	if *labels != "" || !*client {
		roles = append(roles, store.RoleExecute)
	}

	return withClient(ctx, env, func(ctx context.Context, c *Client) error {
		reqCtx, cancel := withTimeout(ctx)
		defer cancel()

		out, err := c.MintEnrollment(reqCtx, api.MintEnrollmentRequest{
			Name: positional[0], Labels: splitLabels(*labels), Roles: roles,
		})
		if err != nil {
			return err
		}

		fmt.Fprintf(env.Stdout, "token    %s\n", out.Token)
		fmt.Fprintf(env.Stdout, "name     %s\n", out.Name)
		fmt.Fprintf(env.Stdout, "roles    %s\n", strings.Join(out.Roles, ", "))
		if len(out.Labels) > 0 {
			fmt.Fprintf(env.Stdout, "labels   %s\n", strings.Join(out.Labels, ", "))
		}
		fmt.Fprintf(env.Stdout, "expires  in %s\n\n", out.Expires)

		// The command to run is the one that matches what was minted. A client
		// that was told to run `je worker run` would start a worker, which is
		// not what it is for.
		redeem := "je worker run"
		if !slices.Contains(out.Roles, store.RoleExecute) {
			redeem = "je identity join"
		}
		fmt.Fprintf(env.Stdout,
			"On that machine:\n  %s --token %s \\\n    --addr %s --ca-pin %s\n\n",
			redeem, out.Token, c.Addr(), out.CAFingerprint)

		// The address is how THIS command reached the control plane, which is
		// not always how the other machine will. Inside a container it is the
		// container's own port and the host needs the published one; across a
		// network it is loopback and useless. The pin travels unchanged either
		// way, so saying which half to check beats printing a confident address
		// somebody pastes and then has to debug.
		fmt.Fprint(env.Stdout,
			"--addr is how this command reached the control plane. If that machine\n"+
				"reaches it by another name or port -- through a published container\n"+
				"port, or over a network -- use that instead. The fingerprint is the\n"+
				"same wherever it is reached from.\n\n")
		fmt.Fprintln(env.Stdout,
			"Shown once. The control plane keeps a hash, so this cannot be printed again --\n"+
				"if it is lost, issue another one.")

		// The consequence, said while it can still be reconsidered. This is the
		// moment a deployment stops accepting anonymous writes, and finding
		// that out from a later `je run` that fails would be exactly the 3am
		// surprise P1 exists to prevent.
		if out.ArmsIdentity {
			fmt.Fprintf(env.Stderr,
				"\nThis is the first client identity on this control plane.\n"+
					"Once it is redeemed, every request that CHANGES something must present\n"+
					"a certificate -- `je run`, `je secret set`, `je source add`, and this\n"+
					"command. Reading is unaffected.\n\n"+
					"Machines that will still need to write:\n"+
					"  beside the control plane   je identity join        (no token needed)\n"+
					"  anywhere else              je enroll <name> --client, then redeem it\n")
		}
		return nil
	})
}

// enrollAt redeems a token against a control plane named directly.
//
// Not withClient: that resolves from this machine's own data directory, and a
// machine that is becoming a worker has none. Being told where to go is the
// whole situation enrollment exists for.
func enrollAt(ctx context.Context, env *Env, addr, token, pin string) error {
	if pin == "" {
		// There is no unpinned path left. A token is a bearer credential, and
		// handing it to whatever answers an address was tolerable only while a
		// plaintext control plane was a thing that existed (D25).
		return fmt.Errorf(
			"--ca-pin is required with --token.\n" +
				"It is the fingerprint printed beside the token by `je enroll`, and it is\n" +
				"what proves the control plane answering this address is the one that\n" +
				"issued the token -- checked before the token is sent.")
	}

	// The control plane is verified BEFORE the token is sent. A token is a
	// bearer credential -- whoever receives it becomes this worker -- so
	// handing it to whatever answers the address would make enrollment the one
	// step where an impostor is free.
	caPEM, err := fetchAuthority(ctx, addr, pin)
	if err != nil {
		return err
	}
	c, err := DialVerified(addr, caPEM)
	if err != nil {
		return err
	}
	return enrollWorker(ctx, env, c, token)
}

// fetchAuthority downloads the control plane's CA over an unverified
// connection and refuses it unless it matches the pin.
//
// Unverified is safe here precisely because nothing is trusted yet and nothing
// secret is sent: the response is a public certificate, and it is discarded
// unless its fingerprint is the one printed by `je enroll` on the other machine.
func fetchAuthority(ctx context.Context, addr, pin string) ([]byte, error) {
	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // checked by fingerprint below, not by chain
			MinVersion:         tls.VersionTLS12,
		}},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/v1/ca", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching the control plane's authority: %w", err)
	}
	defer resp.Body.Close()

	// Checked here because this is the one exchange that happens before
	// anything is trusted, and because a clock that is wrong will otherwise
	// surface much later as a certificate that looks expired (D25).
	if err := ca.CheckClockSkew(resp.Header.Get("Date")); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	got := ca.FingerprintPEM(body)
	if !strings.EqualFold(got, pin) {
		return nil, fmt.Errorf(
			"the control plane at %s is not the one this token was issued by.\n"+
				"  expected  %s\n"+
				"  found     %s\n"+
				"Nothing was sent. Check the address, and do not reuse this token.",
			addr, pin, got)
	}
	return body, nil
}

// enrollWorker redeems a token on the machine that is becoming a worker.
//
// The private key is generated here and never leaves: what crosses the network
// is a public key and a token, and what comes back is a certificate. A control
// plane that is compromised can refuse to issue, and cannot learn this key.
// autoEnroll gives a worker on the control plane's own machine an identity,
// with nobody asked for anything (D25).
//
// Skipped silently when there is nothing to do. No token file means the control
// plane is somewhere else, and a worker there enrolls with a token and a pin
// instead; an identity already present means there is nothing to bootstrap.
func autoEnroll(ctx context.Context, env *Env, target, name string, labels, roles []string) error {
	if _, err := os.Stat(env.Layout.IdentityCert()); err == nil {
		return nil
	}
	token, err := localBootstrapToken(ctx, env)
	if err != nil || token == "" {
		return nil
	}

	// Verified against the CA this machine has, which it can get for the same
	// reason it could get the token. No pin is needed: locality is the proof,
	// and there is no network for anybody to sit in the middle of.
	path, err := authorityPath(env.Layout)
	if err != nil {
		return fmt.Errorf(
			"there is a local enrollment token but no authority to go with it: %w", err)
	}
	caPEM, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	c, err := DialVerified(target, caPEM)
	if err != nil {
		return err
	}
	return enrollWorkerAs(ctx, env, c, token, name, labels, roles)
}

// localBootstrapToken finds the token that lets a worker on this machine enroll
// itself, wherever the control plane beside it keeps one.
//
// Two places, and the second is the point. A native control plane leaves it in
// the data directory they share. A containerised one leaves it inside its own
// volume, which the host cannot see -- and "the control plane is on this
// machine" is no less true for that. Reaching into the container is what makes
// `je worker run` beside a containerised control plane need nothing pasted,
// which is what the two deployment shapes being one product actually requires.
func localBootstrapToken(ctx context.Context, env *Env) (string, error) {
	body, err := os.ReadFile(env.Layout.BootstrapToken())
	if err == nil {
		return strings.TrimSpace(string(body)), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	name := controlPlaneContainer(env.Layout)
	if dockerAvailable() != nil || !containerNamed(ctx, name) {
		// No control plane on this machine. Not an error: this is every worker
		// that lives somewhere else, and it enrolls with a token instead.
		return "", nil
	}
	return copyBootstrapTokenFrom(ctx, name)
}

func enrollWorker(ctx context.Context, env *Env, c *Client, token string) error {
	return enrollWorkerAs(ctx, env, c, token, "", nil, nil)
}

// enrollWorkerAs redeems a token and writes the identity it is issued.
//
// name, labels and roles are honoured only for a bootstrap token, where the
// holder is on the control plane's own machine and names itself. For a minted
// token the control plane ignores all three, because what the identity may be
// was decided when the token was issued (D25).
func enrollWorkerAs(ctx context.Context, env *Env, c *Client, token, name string, labels, roles []string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	out, err := c.Enroll(reqCtx, api.EnrollRequest{
		Token: token, PublicKey: string(pubPEM),
		Name: name, Labels: labels, Roles: roles,
		// Bound at the moment the identity is decided, when this machine
		// already has a key. Empty is ordinary: a machine that has never run
		// `je worker keygen` enrolls without one and registers it later (D25).
		AgeRecipient: ageRecipientOf(env),
	})
	if err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(env.Layout.Data, 0o700); err != nil {
		return err
	}
	files := []struct {
		path string
		body []byte
		mode os.FileMode
	}{
		{env.Layout.IdentityKey(), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600},
		{env.Layout.IdentityCert(), []byte(out.Certificate), 0o644},
		{env.Layout.IdentityCA(), []byte(out.CA), 0o644},
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, f.body, f.mode); err != nil {
			return err
		}
	}

	fmt.Fprintf(env.Stderr, "enrolled as %s; identity written to %s\n",
		certName(out.Certificate), env.Layout.IdentityCert())
	return nil
}

// certName reads back what the control plane decided this identity is called,
// which for a bootstrap enrollment is what was asked for and for a token
// enrollment may not be.
func certName(certPEM string) string {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return "?"
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "?"
	}
	return cert.Subject.CommonName
}
