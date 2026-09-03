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

	"crypto/tls"
	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/ca"
	"io"
	"net/http"
	"time"
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
			"On that machine:\n  je worker run --token %s \\\n    --addr %s --ca-pin %s\n\n",
			out.Token, c.Addr(), out.CAFingerprint)
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
func enrolAt(ctx context.Context, env *Env, addr, token, pin string) error {
	if pin == "" {
		// No pin means a plaintext control plane, which is the pre-TLS shape
		// and still valid on a trusted network (D19).
		c, err := DialAddr(addr)
		if err != nil {
			return err
		}
		return enrolWorker(ctx, env, c, token)
	}

	// The control plane is verified BEFORE the token is sent. A token is a
	// bearer credential -- whoever receives it becomes this worker -- so
	// handing it to whatever answers the address would make enrolment the one
	// step where an impostor is free.
	caPEM, err := fetchAuthority(ctx, addr, pin)
	if err != nil {
		return err
	}
	c, err := DialVerified(addr, caPEM)
	if err != nil {
		return err
	}
	return enrolWorker(ctx, env, c, token)
}

// fetchAuthority downloads the control plane's CA over an unverified
// connection and refuses it unless it matches the pin.
//
// Unverified is safe here precisely because nothing is trusted yet and nothing
// secret is sent: the response is a public certificate, and it is discarded
// unless its fingerprint is the one printed by `je enrol` on the other machine.
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

// enrolWorker redeems a token on the machine that is becoming a worker.
//
// The private key is generated here and never leaves: what crosses the network
// is a public key and a token, and what comes back is a certificate. A control
// plane that is compromised can refuse to issue, and cannot learn this key.
// autoEnrol gives a worker on the control plane's own machine an identity,
// with nobody asked for anything (D25).
//
// Skipped silently when there is nothing to do: no token file means either a
// remote control plane or an older one, and both are cases where the worker
// carries on exactly as it did before. An identity already present means there
// is nothing to bootstrap.
func autoEnrol(ctx context.Context, env *Env, target, name string, labels []string) error {
	if _, err := os.Stat(env.Layout.IdentityCert()); err == nil {
		return nil
	}
	token, err := os.ReadFile(env.Layout.BootstrapToken())
	if err != nil {
		return nil
	}

	// Verified against the CA on disk, which this process can read for the same
	// reason it could read the token. No pin is needed: locality is the proof,
	// and there is no network for anybody to sit in the middle of.
	caPEM, caErr := os.ReadFile(env.Layout.BootstrapCA())
	var c *Client
	if caErr == nil {
		c, err = DialVerified(target, caPEM)
	} else {
		c, err = DialAddr(target)
	}
	if err != nil {
		return err
	}
	return enrolWorkerAs(ctx, env, c, strings.TrimSpace(string(token)), name, labels)
}

func enrolWorker(ctx context.Context, env *Env, c *Client, token string) error {
	return enrolWorkerAs(ctx, env, c, token, "", nil)
}

func enrolWorkerAs(ctx context.Context, env *Env, c *Client, token, name string, labels []string) error {
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
	out, err := c.Enrol(reqCtx, api.EnrolRequest{
		Token: token, PublicKey: string(pubPEM), Name: name, Labels: labels,
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
		{filepath.Join(env.Layout.Data, "ca.crt"), []byte(out.CA), 0o644},
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
// which for a bootstrap enrolment is what was asked for and for a token
// enrolment may not be.
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
