// Package daemon is the thin wrapper that turns the engine library into a
// running process: a listener, an HTTP server, signal handling, and an orderly
// shutdown.
//
// Everything here is about being a process. Anything about being a job engine
// belongs in internal/engine (D18).
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/paths"
)

// Config configures a daemon run.
type Config struct {
	Layout  paths.Layout
	Addr    string
	Version string
	Logger  *slog.Logger

	// TLS serves HTTPS with a certificate the control plane issues itself,
	// from the same authority workers enrol against (D25 step 5).
	//
	// Off by default, and that is deliberate rather than timid: every existing
	// deployment speaks plaintext on a trusted network (D19), and flipping the
	// transport underneath one would be a breaking change dressed as a security
	// improvement. On, a presented client certificate becomes an identity; an
	// absent one is simply nobody, because the CLI and the web client are
	// clients too and read endpoints need no identity.
	TLS bool

	// Ready, if set, is closed once the daemon is listening and the runtime
	// file is published. Tests use it to avoid polling; nothing else needs it.
	Ready chan<- struct{}
}

// shutdownGrace is how long in-flight HTTP requests get to finish.
const shutdownGrace = 5 * time.Second

// Run starts the daemon and blocks until ctx is cancelled or the server fails.
//
// The shape here is the standard one and worth reading closely if Go's
// cancellation idiom is new: ctx is the only stop signal, ListenAndServe runs
// on its own goroutine because it blocks, and the select below waits for
// whichever comes first -- cancellation or a server error.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}

	eng, err := engine.New(engine.Options{
		Layout:  cfg.Layout,
		Logger:  cfg.Logger,
		Version: cfg.Version,
	})
	if err != nil {
		return err
	}
	// Shutdown uses its own context: the one that cancelled us is already
	// done, and engine.Close still needs to write engine.stopped. Using a
	// cancelled context here would leave exactly the hole in the timeline that
	// D16 exists to prevent.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := eng.Close(closeCtx); err != nil {
			cfg.Logger.Error("closing engine", "error", err)
		}
		if err := os.Remove(cfg.Layout.Runtime()); err != nil && !os.IsNotExist(err) {
			cfg.Logger.Warn("removing runtime file", "error", err)
		}
	}()

	if err := eng.Start(ctx); err != nil {
		return err
	}

	// Definitions before schedules: the scheduler builds its table from what
	// is loaded, so an empty load would mean an empty scheduler.
	//
	// Every registered source, not just the local directory (D22), which means
	// a fetched source is re-read on start -- the cadence D22 settles on,
	// manual plus once at boot. A source that cannot be reached is recorded
	// and skipped: its jobs are already rows in the database and keep running
	// from the tree last fetched, so a laptop that wakes without a network
	// keeps working and says why it did not update.
	if _, err := eng.Sync(ctx); err != nil {
		// A broken job file must not stop the daemon. Every other job should
		// keep running, and `je jobs` is where the problem is visible (P1).
		cfg.Logger.Error("loading definitions", "error", err)
	}

	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- eng.RunScheduler(ctx) }()

	// Bind before serving, so that "port already in use" is a startup error
	// the caller sees rather than something logged after we have claimed to
	// have started.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Addr, err)
	}

	if err := WriteRuntime(cfg.Layout.Runtime(), RuntimeInfo{
		Address:   ln.Addr().String(),
		PID:       os.Getpid(),
		Version:   cfg.Version,
		StartedAt: time.Now(),
		TLS:       cfg.TLS,
	}); err != nil {
		ln.Close()
		return err
	}

	srv := &http.Server{
		Handler:           api.New(eng, cfg.Logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second, // cheap protection against a stuck client
	}

	serve := srv.Serve
	if cfg.TLS {
		authority, err := eng.Authority()
		if err != nil {
			ln.Close()
			return fmt.Errorf("preparing the certificate authority: %w", err)
		}
		host, _, splitErr := net.SplitHostPort(cfg.Addr)
		if splitErr != nil || host == "" || host == "0.0.0.0" || host == "::" {
			// A wildcard bind has no name of its own to certify. Loopback is
			// always added, and anything else is reached by an address this
			// process cannot know -- so it is named at the client instead.
			host = ""
		}
		var hosts []string
		if host != "" {
			hosts = append(hosts, host)
		}
		tlsConfig, err := authority.ServerTLS(hosts)
		if err != nil {
			ln.Close()
			return err
		}
		srv.TLSConfig = tlsConfig
		serve = func(l net.Listener) error { return srv.ServeTLS(l, "", "") }
		cfg.Logger.Info("serving TLS", "client_certs", "verified if presented")
	}

	serveErr := make(chan error, 1) // buffered: the goroutine must not block if we already returned
	go func() {
		err := serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil // an expected shutdown is not a failure
		}
		serveErr <- err
	}()

	cfg.Logger.Info("listening", "addr", ln.Addr().String())
	if cfg.Ready != nil {
		close(cfg.Ready)
	}

	select {
	case err := <-serveErr:
		return err
	case err := <-schedulerDone:
		// The scheduler only returns on cancellation or a failure it cannot
		// recover from. Either way the daemon has nothing left to do.
		if err != nil {
			return fmt.Errorf("scheduler stopped: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Shutdown timed out with requests still in flight. Say so, and
			// carry on shutting down -- the deferred engine.Close matters more.
			cfg.Logger.Warn("graceful shutdown timed out", "error", err)
		}
		// Wait for in-flight jobs to finish being recorded before the deferred
		// engine.Close releases the database. Without this, a run that was
		// executing at shutdown could lose its terminal status -- the hole in
		// the timeline that `interrupted` exists to fill.
		select {
		case <-schedulerDone:
		case <-time.After(shutdownGrace):
			cfg.Logger.Warn("scheduler did not stop within the grace period")
		}
		return nil
	}
}
