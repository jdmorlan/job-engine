package cli

import (
	"context"
	"filippo.io/age"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/service"
	"github.com/jdmorlan/job-engine/internal/worker"
)

func init() {
	register(&Command{
		Name:  "worker",
		Args:  "run|join|status|remove|keygen|runtimes",
		Usage: "a worker: the thing that actually executes jobs",
		Long: "A worker executes jobs. The control plane never does (D20/C11), so a\n" +
			"deployment with no worker runs nothing at all -- `je status` says so.\n\n" +
			"Workers advertise capability labels, and a job's `runs_on` picks one.\n" +
			"A worker on your Mac advertising `macos` is how a job that has to talk\n" +
			"to Shortcuts reaches the machine that can.\n\n" +
			"It holds no state and opens no ports: it dials the control plane and\n" +
			"keeps asking for work, which is why it works from a laptop behind NAT.\n\n" +
			"A worker on another machine needs an identity before it can talk to\n" +
			"anything: `je enroll <name>` on the control plane prints a token and a\n" +
			"fingerprint, and `je worker run --token <t> --ca-pin <fp> --addr <a>`\n" +
			"here redeems them. A worker sharing a machine with the control plane\n" +
			"does that by itself, with nothing to paste.\n\n" +
			"subcommands:\n" +
			"  run       run it in the foreground, in this terminal\n" +
			"  join      register it with launchd or systemd, attached to a control plane\n" +
			"  status    is it registered, and is it up\n" +
			"  remove    unregister it; nothing else on this machine is touched\n" +
			"  keygen    create this machine's key for reading encrypted secrets\n" +
			"  runtimes  which languages this machine can prepare, and how to add one\n\n" +
			"`join` rather than `install` because a worker attaches to a control plane\n" +
			"that already exists -- with no argument it joins the one this data\n" +
			"directory records, which is the local case.\n\n" +
			"`je workers` lists the ones already attached.",
		Local: true,
		Run:   runWorker,
	})
	register(&Command{
		Name:  "workers",
		Args:  "[restart|upgrade <name>]",
		Usage: "list the workers attached to the control plane, and direct them",
		Long: "With no arguments, what is attached and what it can run.\n\n" +
			"`je workers restart <name>` and `je workers upgrade <name>` reach a\n" +
			"worker on another machine. They are requests rather than commands: the\n" +
			"worker acts the next time it checks in, after finishing whatever it is\n" +
			"running -- so a restart never costs an in-flight job.\n\n" +
			"What brings a worker back is whatever supervises it: a service, a\n" +
			"container restart policy, a Kubernetes pod. One started by hand in a\n" +
			"terminal simply stops, because nothing was supervising it.\n\n" +
			"`upgrade` replaces that worker's own binary first -- the same download\n" +
			"and checksum check `je upgrade` performs here, asked for from there.",
		Run: runWorkers,
	})
}

func runWorker(ctx context.Context, env *Env, args []string) error {
	cmd := commands["worker"]
	fs := newFlagSet(cmd, env)
	name := fs.String("name", defaultWorkerName(), "what to call this worker")
	labels := fs.String("labels", jobdef.DefaultRunsOn, "comma-separated capability labels")
	addr := fs.String("addr", "", "control plane address (default: the one this data dir records)")
	concurrency := fs.Int("concurrency", 0, "how many jobs to run at once")
	verbose := fs.Bool("v", false, "log at debug level")
	useDocker := fs.Bool("docker", false, "join as a container instead of a native service")
	native := fs.Bool("native", false, "join as a native service (launchd or systemd)")
	printOnly := fs.Bool("print", false, "print what would be done, and do nothing")
	token := fs.String("token", "", "an enrollment token from `je enroll` on the control plane")
	caPin := fs.String("ca-pin", "", "the control plane's CA fingerprint, printed beside the token")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return usagef("usage: je worker run|join|status|remove")
	}

	switch positional[0] {
	case "runtimes":
		if len(positional) != 1 {
			return usagef("unexpected argument %q", positional[1])
		}
		return runWorkerRuntimes(ctx, env)
	case "runtime":
		if len(positional) != 3 || positional[1] != "install" {
			return usagef("usage: je worker runtime install <language>")
		}
		return runWorkerRuntimeInstall(ctx, env, positional[2])
	case "keygen":
		if len(positional) != 1 {
			return usagef("unexpected argument %q", positional[1])
		}
		return runWorkerKeygen(ctx, env)
	case "run", "join":
		if len(positional) != 1 {
			return usagef("unexpected argument %q", positional[1])
		}
	case "status":
		if len(positional) != 1 {
			return usagef("unexpected argument %q", positional[1])
		}
		return componentStatus(ctx, env, service.Worker)
	case "remove":
		if len(positional) != 1 {
			return usagef("unexpected argument %q", positional[1])
		}
		return removeComponent(ctx, env, service.Worker)
	default:
		return usagef("unknown subcommand %q; expected run, join, status, remove, "+
			"keygen, runtimes or `runtime install <language>`", positional[0])
	}

	target := *addr
	if target == "" {
		// Same resolution the CLI uses, so a worker on this machine finds the
		// control plane without being told twice.
		resolved, err := controlPlaneAddr(env)
		switch {
		case err == nil:
			target = resolved
		case *printOnly:
			// Printing must not require a running control plane: the whole
			// point is to read the command before anything exists.
			target = daemon.DefaultAddr
		default:
			return adviseNoControlPlane(env, err)
		}
	}
	// A recorded 0.0.0.0 is a bind address, not a destination, and nothing
	// certifies it -- so a TLS client checking the hostname would reject a
	// certificate that is otherwise perfectly correct (D25).
	target = dialable(target)

	// Redeeming happens once the address is resolved and before anything else:
	// `run` and `join` both want an identity in place, and enrolling is the
	// step that puts one there.
	//
	// It uses `target` rather than the CLI's own resolution, because a machine
	// that is becoming a worker has no control plane of its own to look up --
	// that is the entire situation enrollment exists for.
	if *token != "" {
		if err := enrollAt(ctx, env, target, *token, *caPin); err != nil {
			return err
		}
	} else if positional[0] == "run" {
		// No token asked for. If this worker shares a machine with the control
		// plane it can enroll itself, which is what keeps `je up` and
		// `docker compose up` at zero extra steps (D25).
		if err := autoEnroll(ctx, env, target, *name, splitLabels(*labels), nil); err != nil {
			return err
		}
	}

	if positional[0] == "join" {
		return joinWorker(ctx, env, workerJoin{
			name:        *name,
			addr:        target,
			labels:      splitLabels(*labels),
			concurrency: *concurrency,
			mode: installMode{
				docker: *useDocker, native: *native, printOnly: *printOnly,
			},
		})
	}

	// An enrolled worker presents what it was issued; one that never enrolled
	// connects anonymously over the same transport. Presence of the files is
	// the switch rather than a flag, because a machine that has an identity has
	// no reason not to use it, and one that does not cannot (D25).
	client, err := dialControlPlane(env, target)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	w, err := worker.New(worker.Options{
		Name:         *name,
		Labels:       splitLabels(*labels),
		Concurrency:  *concurrency,
		DataDir:      env.Layout.Data,
		CacheDir:     env.Layout.Data,
		ToolchainBin: env.Layout.ToolchainBin(),
		IdentityFile: env.Layout.AgeIdentity(),
		Version:      env.Version,
		Client:       client,
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	logger.Info("connecting", "control_plane", client.Addr())
	return w.Run(ctx)
}

func runWorkers(ctx context.Context, env *Env, args []string) error {
	cmd := commands["workers"]
	fs := newFlagSet(cmd, env)
	extra, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		switch extra[0] {
		case "restart", "upgrade":
			if len(extra) != 2 {
				return usagef("usage: je workers %s <name>", extra[0])
			}
			return directWorker(ctx, env, extra[1], extra[0])
		default:
			return usagef("unknown subcommand %q; expected restart or upgrade", extra[0])
		}
	}

	return withClient(ctx, env, func(ctx context.Context, c *Client) error {
		listCtx, cancel := withTimeout(ctx)
		defer cancel()

		workers, err := c.Workers(listCtx)
		if err != nil {
			return err
		}
		if len(workers) == 0 {
			// C8: this is the single most important diagnostic in the system.
			// A control plane with no worker runs nothing, and the sentence
			// saying so is worth more than an empty table.
			fmt.Fprintln(env.Stdout,
				"no workers attached -- nothing will run.\n\n"+
					"Start one:  je worker run\n"+
					"            docker compose up -d   (unattended)")
			return nil
		}

		// The control plane's own version, so skew can be named rather than
		// left for somebody to spot by comparing two columns themselves (D24).
		// A health call that fails is not worth failing the listing over: the
		// table is still true, it just cannot mark anything.
		var plane string
		if health, err := c.Health(listCtx); err == nil {
			plane = health.Version
		}

		st := env.Style
		stale := 0
		tw := env.table()
		fmt.Fprintln(tw, st.Header("NAME\tROLES\tLABELS\tVERSION\tSESSION"))
		for _, w := range workers {
			session := st.Bad("offline")
			if w.Online {
				session = st.Good("online") + " " + st.Muted(humanAge(w.LastSeenAt))
			} else if w.GoneAt != nil {
				session = st.Bad("offline") + " " + st.Muted("since "+humanAge(*w.GoneAt))
			}
			version := w.Version
			if version == "" {
				version = "unknown"
			}
			if plane != "" && !sameVersion(w.Version, plane) {
				version = st.Warn(version + " *")
				stale++
			} else {
				version = st.Muted(version)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				w.Name, st.Muted(strings.Join(w.Roles, ", ")),
				strings.Join(w.Labels, ", "), version, session)
		}
		if err := tw.Flush(); err != nil {
			return err
		}

		if stale > 0 {
			// C10 refuses skew at registration, but only there -- a worker that
			// registered before an upgrade keeps claiming at its old version,
			// because nothing re-checks it (D24). Saying so is the whole of
			// phase 1: the fix is a restart, and you cannot ask for one you
			// cannot see.
			fmt.Fprintf(env.Stdout, "\n%s\n%s\n",
				st.Warn(fmt.Sprintf("* out of date -- the control plane is %s.", plane)),
				st.Muted("  A worker is only version-checked when it registers, so one that was\n"+
					"  running before the upgrade keeps claiming work at its old version.\n"+
					"  Restart it to pick up "+plane+":  ")+
					st.Cmd("je worker run")+st.Muted("   (or restart its container)"))
		}
		return nil
	})
}

// controlPlaneAddr resolves where the control plane is listening.
//
// Deliberately the same resolution every other command uses. It had its own
// copy, which knew about the runtime file and not the endpoint file -- so a
// worker could not find a containerised control plane that `je status` had no
// trouble with. Two lookups for one question is how they end up disagreeing.
func controlPlaneAddr(env *Env) (string, error) {
	return resolveAddr(env.Layout)
}

// defaultWorkerName is the machine's name, which is what a person calls it.
func defaultWorkerName() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "worker"
}

func splitLabels(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// runWorkerKeygen creates this machine's age key and prints the public half.
//
// The private key is written and never shown: D10's rule that the CLI does not
// print secret material applies to the key that reads secrets at least as much
// as to the secrets themselves. What is printed is the recipient, which is
// public by construction and is the thing you paste into a source's secrets
// file to let this machine read it (D25).
func runWorkerKeygen(ctx context.Context, env *Env) error {
	path := ageIdentityPath(env)

	existing, err := readAgeIdentity(env)
	if err == nil {
		// Refused rather than overwritten. Replacing this key silently would
		// make every secret encrypted to it unreadable, with no way back and
		// nothing said.
		//
		// Registering it again is not refused, though: a machine whose key
		// predates the control plane knowing about keys has one to tell it
		// about, and that is the whole point of the second half of this
		// command (D25).
		fmt.Fprintf(env.Stderr, "%s already exists; keeping it.\n", path)
		return registerAgeKey(ctx, env, existing.Recipient().String())
	} else if !os.IsNotExist(err) {
		return err
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(id.String()+"\n"), 0o600); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "wrote %s\n", path)
	fmt.Fprintf(env.Stdout, "public key  %s\n\n", id.Recipient())
	return registerAgeKey(ctx, env, id.Recipient().String())
}

// registerAgeKey tells the control plane which key this identity reads with, so
// a recipient list can name the machine instead of the key (D25).
//
// Best-effort by design. A machine that cannot reach a control plane, or that
// has no identity yet, still has a usable key on disk -- and the enrollment it
// performs later carries the key with it, so the binding happens anyway. Making
// this fatal would mean `je worker keygen` could not be run before enrolling,
// which is the order somebody will naturally use.
func registerAgeKey(ctx context.Context, env *Env, recipient string) error {
	client, err := Connect(env.Layout)
	if err != nil {
		return explainUnregisteredKey(env, recipient,
			"there is no control plane to tell about it here")
	}
	regCtx, cancel := withTimeout(ctx)
	defer cancel()

	name, err := client.RegisterAgeKey(regCtx, recipient)
	if err != nil {
		return explainUnregisteredKey(env, recipient, err.Error())
	}

	fmt.Fprintf(env.Stdout, "registered as %s's key on the control plane.\n\n", name)
	fmt.Fprintf(env.Stdout,
		"Let this machine read a source's secrets -- by name, not by pasting a key:\n"+
			"  je secret recipients add --source <src> %s\n", name)
	return nil
}

func explainUnregisteredKey(env *Env, recipient, why string) error {
	fmt.Fprintf(env.Stderr,
		"\nThe key is on disk, but the control plane does not know about it: %s.\n"+
			"This machine can still run jobs that need no secrets.\n\n"+
			"When it can reach one, run `je worker keygen` again to register it --\n"+
			"or enroll, which carries the key with it:\n"+
			"  je worker run --token <t> --ca-pin <fp> --addr <host:port>\n\n"+
			"To add it by hand meanwhile:\n"+
			"  je secret recipients add --source <src> %s\n", why, recipient)
	return nil
}

// ageIdentityPath is where this machine's secret-reading key lives.
func ageIdentityPath(env *Env) string { return env.Layout.AgeIdentity() }

// ensureAgeIdentity loads this machine's key, creating one if there is none.
//
// Creating rather than instructing, because being told to run `je worker
// keygen` is a step we made somebody type instead of doing. A keypair is not a
// destructive act and it costs nothing: the only reason to refuse would be if
// there were already one, and that case is still refused where it matters --
// `je worker keygen` will not overwrite a key, because replacing it makes every
// secret encrypted to it unreadable.
//
// It is loud about it. A new identity is a fact worth seeing go by, and the
// public key is what somebody has to add as a recipient elsewhere.
func ensureAgeIdentity(env *Env) (*age.X25519Identity, error) {
	id, err := readAgeIdentity(env)
	if err == nil {
		return id, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	path := ageIdentityPath(env)
	id, err = age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(id.String()+"\n"), 0o600); err != nil {
		return nil, err
	}
	fmt.Fprintf(env.Stderr,
		"je: this machine had no key to encrypt with, so one was made\n"+
			"    %s\n"+
			"    public key  %s\n",
		path, id.Recipient())
	return id, nil
}

// readAgeIdentity loads this machine's secret-reading key.
func readAgeIdentity(env *Env) (*age.X25519Identity, error) {
	body, err := os.ReadFile(ageIdentityPath(env))
	if err != nil {
		return nil, err
	}
	return age.ParseX25519Identity(strings.TrimSpace(string(body)))
}

// ageRecipientOf is the public half of this machine's key, or "" when it has
// none. Used at enrollment, where a key that exists should be bound at the
// moment the identity is decided rather than in a second step (D25).
func ageRecipientOf(env *Env) string {
	id, err := readAgeIdentity(env)
	if err != nil {
		return ""
	}
	return id.Recipient().String()
}

// dialControlPlane connects a worker to the control plane.
//
// The transport is settled -- HTTPS, verified against the control plane's own
// authority (D25) -- so the only question left is whether this machine has an
// identity to present. An enrolled worker presents one and is that worker
// everywhere it matters; one that has not enrolled connects anonymously, which
// the control plane still accepts and which is exactly the pre-D25 guarantee.
//
// Missing the authority is a hard error rather than a fallback. There is
// nothing to fall back to, and saying so names the fix (enroll) instead of
// producing a connection failure somebody has to interpret.
func dialControlPlane(env *Env, target string) (*worker.Client, error) {
	caPath, err := authorityPath(env.Layout)
	if err != nil {
		return nil, err
	}

	cert, key := env.Layout.IdentityCert(), env.Layout.IdentityKey()
	for _, path := range []string{cert, key} {
		if _, err := os.Stat(path); err != nil {
			return worker.DialCA(target, caPath)
		}
	}
	return worker.DialTLS(target, cert, key, caPath)
}

// directWorker asks a worker elsewhere to restart, or to upgrade and restart.
func directWorker(ctx context.Context, env *Env, name, directive string) error {
	return withClient(ctx, env, func(ctx context.Context, c *Client) error {
		reqCtx, cancel := withTimeout(ctx)
		defer cancel()

		if err := c.DirectWorker(reqCtx, name, directive); err != nil {
			return err
		}
		// Said as a request, because that is what it is: the worker acts when
		// it next checks in, and one that is offline acts when it returns.
		fmt.Fprintf(env.Stdout, "asked %s to %s\n", name, directive)
		fmt.Fprintf(env.Stderr,
			"It acts on this at its next heartbeat, after finishing what it is running.\n"+
				"Watch for it:  je workers\n")
		return nil
	})
}
