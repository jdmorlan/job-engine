package cli

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

func init() {
	register(&Command{
		Name:  "identity",
		Args:  "join|show",
		Usage: "this machine's certificate: who the control plane thinks you are",
		Long: "A certificate for the machine you type on, so that `je run` is attributed\n" +
			"to somebody rather than to whatever the request body claimed (D25).\n\n" +
			"It is not a login. There is no session, no password and nothing to\n" +
			"expire out from under you: a key is generated here, the control plane\n" +
			"signs the public half, and the certificate is renewed automatically\n" +
			"while it is still valid.\n\n" +
			"subcommands:\n" +
			"  join    redeem a token from `je enroll --client`, and store the identity\n" +
			"  show    what this machine presents, and what the control plane makes of it\n\n" +
			"Beside the control plane itself, `je identity join` needs no token: it\n" +
			"reads the one left in the data directory, which it can only do if it\n" +
			"already has the access that would let it read the CA key.\n\n" +
			"A worker gets its identity from `je worker run` instead. A machine that\n" +
			"is both keeps one certificate carrying both roles.",
		Run: runIdentity,
	})
}

func runIdentity(ctx context.Context, env *Env, args []string) error {
	cmd := commands["identity"]
	fs := newFlagSet(cmd, env)
	addr := fs.String("addr", "", "control plane address (default: the one this data dir records)")
	token := fs.String("token", "", "a token from `je enroll <name> --client`")
	caPin := fs.String("ca-pin", "", "the control plane's CA fingerprint, printed beside the token")
	name := fs.String("name", defaultWorkerName(), "what to call this machine, when it names itself")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: je identity join|show")
	}

	switch positional[0] {
	case "show":
		return identityShow(ctx, env)
	case "join":
		return identityJoin(ctx, env, *addr, *token, *caPin, *name)
	default:
		return usagef("unknown subcommand %q; expected join or show", positional[0])
	}
}

// identityJoin obtains a certificate for this machine.
//
// Two shapes, and which one applies is decided by what is available rather than
// by a flag: a token redeems against a control plane somewhere else, and no
// token means the control plane is here and left one in its own data directory.
func identityJoin(ctx context.Context, env *Env, addr, token, pin, name string) error {
	if _, err := os.Stat(env.Layout.IdentityCert()); err == nil {
		// Refused rather than replaced. An identity is the thing runs are
		// attributed to, and silently issuing a second one would leave the
		// first still valid and nothing saying which is in use.
		return fmt.Errorf("this machine already has an identity at %s (%s).\n"+
			"To replace it, remove that file and its key, then join again.",
			env.Layout.IdentityCert(), identityName(env))
	}

	target := addr
	if target == "" {
		resolved, err := controlPlaneAddr(env)
		if err != nil {
			return adviseNoControlPlane(err)
		}
		target = resolved
	}
	target = dialable(target)

	if token != "" {
		return enrollAt(ctx, env, target, token, pin)
	}

	// No token: the local case. autoEnroll is silent when there is nothing to
	// read, which is right for a worker starting up and wrong here -- somebody
	// typed this and is owed an answer either way.
	if _, err := os.Stat(env.Layout.BootstrapToken()); err != nil {
		return fmt.Errorf(
			"no --token given, and no local enrollment token at %s.\n\n"+
				"That token exists only beside a running control plane. If this machine\n"+
				"is not one, get a token from the machine that is:\n"+
				"  je enroll %s --client        (there)\n"+
				"  je identity join --token <t> --ca-pin <fp> --addr <host:port>   (here)",
			env.Layout.BootstrapToken(), name)
	}
	if err := autoEnroll(ctx, env, target, name, nil, []string{store.RoleClient}); err != nil {
		return err
	}
	return identityShow(ctx, env)
}

// identityShow reports what this machine presents and what the control plane
// makes of it.
//
// Both halves matter and they can disagree: the certificate is what this
// machine holds, and the roles are what the control plane recorded when it
// issued one. A certificate that is still valid for an identity the control
// plane has forgotten is exactly the state worth being able to see.
func identityShow(ctx context.Context, env *Env) error {
	cert, err := readIdentityCert(env)
	if err != nil {
		return fmt.Errorf("this machine has no identity: %w.\n\n"+
			"Get one:  je identity join        (beside the control plane)\n"+
			"          je enroll <name> --client, then redeem the token here", err)
	}

	fmt.Fprintf(env.Stdout, "name         %s\n", cert.Subject.CommonName)
	fmt.Fprintf(env.Stdout, "fingerprint  %s\n", engine.Fingerprint(cert.Raw)[:16])
	fmt.Fprintf(env.Stdout, "expires      %s\n", humanUntil(cert.NotAfter))
	fmt.Fprintf(env.Stdout, "file         %s\n", env.Layout.IdentityCert())

	// The control plane's view, when it can be reached. Not an error if it
	// cannot: everything above is true of this machine's disk and is worth
	// printing on its own.
	client, err := Connect(env.Layout)
	if err != nil {
		return nil
	}
	listCtx, cancel := withTimeout(ctx)
	defer cancel()
	workers, err := client.Workers(listCtx)
	if err != nil {
		return nil
	}
	for _, w := range workers {
		if w.Name != cert.Subject.CommonName {
			continue
		}
		fmt.Fprintf(env.Stdout, "roles        %s\n", strings.Join(w.Roles, ", "))
		if len(w.Labels) > 0 {
			fmt.Fprintf(env.Stdout, "labels       %s\n", strings.Join(w.Labels, ", "))
		}
		if !slices.Contains(w.Roles, store.RoleClient) {
			fmt.Fprintf(env.Stdout,
				"\nThis identity is a worker, not a client. It can still write, because\n"+
					"any certificate this authority signed is somebody -- but `je enroll\n"+
					"--client` is what marks a machine as a person at a terminal.\n")
		}
		return nil
	}
	fmt.Fprintf(env.Stdout,
		"\nThe control plane at %s has no record of %q.\n"+
			"The certificate is real and it will still be accepted -- it was signed by\n"+
			"that authority -- but the row it was issued alongside is gone.\n",
		client.Addr(), cert.Subject.CommonName)
	return nil
}

func readIdentityCert(env *Env) (*x509.Certificate, error) {
	body, err := os.ReadFile(env.Layout.IdentityCert())
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, fmt.Errorf("%s is not a certificate", env.Layout.IdentityCert())
	}
	return x509.ParseCertificate(block.Bytes)
}

// identityName is the name in this machine's certificate, for error messages
// that want to say which identity is in the way.
func identityName(env *Env) string {
	cert, err := readIdentityCert(env)
	if err != nil {
		return "unreadable"
	}
	return cert.Subject.CommonName
}

// humanUntil renders a deadline the way a person reads one. Certificates here
// live a day, so "in 19h" is the useful form and a date is not.
func humanUntil(t time.Time) string {
	d := time.Until(t)
	switch {
	case d < 0:
		return "expired " + humanAge(t)
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}
