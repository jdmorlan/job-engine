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
	"path/filepath"
	"strings"

	"github.com/jdmorlan/job-engine/internal/api"
)

func init() {
	register(&Command{
		Name:  "enrol",
		Args:  "<worker-name>",
		Usage: "issue a one-time token that lets one machine become a worker",
		Long: "Run this where the control plane is, then redeem the token on the\n" +
			"machine that is becoming a worker:\n\n" +
			"  je enrol macbook --labels macos     (here)\n" +
			"  je worker join --token <token>      (there)\n\n" +
			"The name and labels are decided HERE, not by the machine redeeming\n" +
			"the token. That is the point: a label is a capability, and a worker\n" +
			"that advertises its own can grant itself whatever a label gates.\n\n" +
			"The token is single-use and short-lived. It is a bearer credential --\n" +
			"whoever holds it becomes this worker -- so it is shown once and the\n" +
			"control plane keeps only a hash.",
		Run: runEnrol,
	})
}

func runEnrol(ctx context.Context, env *Env, args []string) error {
	cmd := commands["enrol"]
	fs := newFlagSet(cmd, env)
	labels := fs.String("labels", "", "comma-separated capabilities this worker may advertise")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: je enrol <worker-name> [--labels a,b]")
	}

	return withClient(ctx, env, func(ctx context.Context, c *Client) error {
		reqCtx, cancel := withTimeout(ctx)
		defer cancel()

		out, err := c.MintEnrolment(reqCtx, api.MintEnrolmentRequest{
			Name: positional[0], Labels: splitLabels(*labels),
		})
		if err != nil {
			return err
		}

		fmt.Fprintf(env.Stdout, "token    %s\n", out.Token)
		fmt.Fprintf(env.Stdout, "worker   %s\n", out.Name)
		fmt.Fprintf(env.Stdout, "labels   %s\n", strings.Join(out.Labels, ", "))
		fmt.Fprintf(env.Stdout, "expires  in %s\n\n", out.Expires)
		fmt.Fprintf(env.Stdout,
			"On that machine:\n  je worker join --token %s --addr %s\n\n",
			out.Token, c.Addr())
		fmt.Fprintln(env.Stdout,
			"Shown once. The control plane keeps a hash, so this cannot be printed again --\n"+
				"if it is lost, issue another one.")
		return nil
	})
}

// enrolAt redeems a token against a control plane named directly.
//
// Not withClient: that resolves from this machine's own data directory, and a
// machine that is becoming a worker has none. Being told where to go is the
// whole situation enrolment exists for.
func enrolAt(ctx context.Context, env *Env, addr, token string) error {
	c, err := DialAddr(addr)
	if err != nil {
		return err
	}
	return enrolWorker(ctx, env, c, token)
}

// enrolWorker redeems a token on the machine that is becoming a worker.
//
// The private key is generated here and never leaves: what crosses the network
// is a public key and a token, and what comes back is a certificate. A control
// plane that is compromised can refuse to issue, and cannot learn this key.
func enrolWorker(ctx context.Context, env *Env, c *Client, token string) error {
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
	out, err := c.Enrol(reqCtx, api.EnrolRequest{Token: token, PublicKey: string(pubPEM)})
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
		{filepath.Join(env.Layout.Data, "ca.crt"), []byte(out.CA), 0o644},
	}
	for _, f := range files {
		if err := os.WriteFile(f.path, f.body, f.mode); err != nil {
			return err
		}
	}

	fmt.Fprintf(env.Stdout, "enrolled; identity written to %s\n", env.Layout.IdentityCert())
	return nil
}
