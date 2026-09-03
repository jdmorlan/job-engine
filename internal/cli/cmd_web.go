package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/webui"
)

const defaultWebAddr = "127.0.0.1:7621"

func init() {
	register(&Command{
		Name:  "web",
		Args:  "run|start|stop",
		Usage: "the web client: history, chains and schedules in a browser",
		Long: "A browser client for the same API the CLI uses (D15/D23). It holds no\n" +
			"state, owns no database, and every capability it has is an endpoint --\n" +
			"which is why it is additive rather than a second implementation.\n\n" +
			"It renders what a terminal renders worse: duration trends, the waiting\n" +
			"view as a graph, and the chain topology as a canvas rather than a list.\n\n" +
			"subcommands:\n" +
			"  run     serve it in the foreground, in this terminal\n" +
			"  start   run it as a container, restarting until you stop it\n" +
			"  stop    stop and remove that container\n\n" +
			"Needs a control plane to talk to and nothing else. With no --control-plane\n" +
			"it uses the one this data directory records, which is the local case.",
		Run: runWeb,
	})
}

func runWeb(ctx context.Context, env *Env, args []string) error {
	cmd := commands["web"]
	fs := newFlagSet(cmd, env)
	addr := fs.String("addr", defaultWebAddr, "address to serve the client on")
	target := fs.String("control-plane", "", "control plane address (default: the one this data dir records)")
	printOnly := fs.Bool("print", false, "print what would be done, and do nothing")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return usagef("usage: je web run|start|stop")
	}
	if len(positional) > 1 {
		return usagef("unexpected argument %q", positional[1])
	}

	switch positional[0] {
	case "stop":
		removed, err := stopContainer(ctx, "web")
		if err != nil {
			return err
		}
		if !removed {
			fmt.Fprintln(env.Stdout, "no web container was running")
			return nil
		}
		fmt.Fprintln(env.Stdout, "stopped the web client")
		return nil
	case "run", "start":
	default:
		return usagef("unknown subcommand %q", positional[0])
	}

	base, err := webTarget(env, *target)
	if err != nil {
		return err
	}

	if positional[0] == "start" {
		return startWebContainer(ctx, env, *addr, base, *printOnly)
	}
	return serveWeb(ctx, env, *addr, base)
}

// webTarget resolves the control plane the client will talk to, preferring what
// was asked for and falling back to what this data directory records -- the same
// precedence every other command uses.
func webTarget(env *Env, flag string) (*url.URL, error) {
	addr := flag
	if addr == "" {
		resolved, err := resolveAddr(env.Layout)
		if err != nil {
			return nil, adviseNoControlPlane(err)
		}
		addr = resolved
	}
	return url.Parse(withScheme(addr))
}

func withScheme(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

func serveWeb(ctx context.Context, env *Env, addr string, base *url.URL) error {
	handler, built, err := webui.Handler(base)
	if err != nil {
		return err
	}
	if !built {
		// Serving the placeholder is still worth doing -- the page says what to
		// run -- but the terminal should not pretend everything is fine.
		fmt.Fprintln(env.Stderr, "warning: this binary carries no web client; run `make web-build && make build`")
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	fmt.Fprintf(env.Stdout, "web client    http://%s\n", ln.Addr())
	fmt.Fprintf(env.Stdout, "control plane %s\n", base)
	fmt.Fprintln(env.Stdout, "\nctrl-c to stop")

	srv := &http.Server{Handler: handler}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func startWebContainer(ctx context.Context, env *Env, addr string, base *url.URL, printOnly bool) error {
	// printOnly is passed through so that --print stays readable on a dev
	// build, which has no published image to name.
	image, err := dockerImage(env, installMode{printOnly: printOnly})
	if err != nil {
		// The shared message offers --native, which the other components have
		// and this one does not: a web client with nothing to serve is not a
		// thing to install, it is `je web run`.
		return fmt.Errorf("this is a %s build, which has no published image.\n"+
			"Install a release (see the README), or serve it here with: je web run", env.Version)
	}
	host, port := splitHostPort(addr)
	if host == "" {
		host = "127.0.0.1"
	}
	// Inside the network the control plane answers to its container name, so a
	// loopback address on the host means nothing to this container.
	target, network := workerTarget(ctx, base.Host)

	spec := dockerSpec{
		component: "web",
		image:     image,
		args:      []string{"--addr", "0.0.0.0:" + port, "--control-plane", target},
		ports:     []string{host + ":" + port + ":" + port},
		network:   network,
	}
	if printOnly {
		fmt.Fprintln(env.Stdout, spec)
		return nil
	}
	if err := dockerAvailable(); err != nil {
		return err
	}
	if err := spec.start(ctx); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "web client running at http://%s:%s\n", host, port)
	fmt.Fprintln(env.Stdout, "\nstop it with: je web stop")
	return nil
}
