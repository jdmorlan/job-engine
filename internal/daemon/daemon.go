// Package daemon is the thin wrapper that turns the engine library into a
// running process: a listener, an HTTPS server, signal handling, and an orderly
// shutdown.
//
// The transport is not a choice. There was a `--tls` flag and a plaintext
// listener beside it, and D25 ends with removing both: a control plane serves
// HTTPS from an authority it owns, and the trust boundary is the certificate
// rather than the network (D19). Nothing here is configurable about that, so
// there is no deployment that is one flag away from being the old thing.
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

	// GitHubAPI overrides where the GitHub API is, for GitHub Enterprise and
	// for tests that serve a repository locally. Empty means api.github.com.
	GitHubAPI string

	// TLSHosts are additional names the control plane will be reached by, for
	// its own certificate.
	//
	// A wildcard bind has no name of its own, and the address a client uses is
	// not something this process can discover: in Compose it is the service
	// name, in a cluster the Service DNS name, on a LAN an IP. So it is told.
	TLSHosts []string

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
		Layout:    cfg.Layout,
		Logger:    cfg.Logger,
		Version:   cfg.Version,
		GitHubAPI: cfg.GitHubAPI,
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
		TLS:       true,
	}); err != nil {
		ln.Close()
		return err
	}

	// A worker on this machine enrolls with no token of its own: it reads this
	// one, which it can only do if it already has the access that would let it
	// read the CA key (D25). Removed on the way out so a stale file cannot
	// outlive the process that honoured it.
	//
	// A failure here is fatal rather than logged, which it was not while the
	// transport was optional. There is no longer a plaintext path to fall back
	// to: a control plane that cannot publish this is one that no local worker
	// can attach to, and starting anyway would produce a system that looks up
	// and runs nothing (C11).
	if err := publishBootstrap(eng, cfg); err != nil {
		ln.Close()
		return fmt.Errorf("preparing local enrollment: %w", err)
	}
	// Only the token goes; the authority stays, because a worker that enrolled
	// needs it to keep verifying this control plane after it restarts.
	defer os.Remove(cfg.Layout.BootstrapToken())

	srv := &http.Server{
		Handler:           api.New(eng, cfg.Logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second, // cheap protection against a stuck client
	}

	authority, err := eng.Authority()
	if err != nil {
		ln.Close()
		return fmt.Errorf("preparing the certificate authority: %w", err)
	}
	hosts := append([]string(nil), cfg.TLSHosts...)
	if host, _, splitErr := net.SplitHostPort(cfg.Addr); splitErr == nil &&
		host != "" && host != "0.0.0.0" && host != "::" {
		hosts = append(hosts, host)
	}
	// The machine's own name, which in a container is usually the name other
	// containers reach it by. Cheap, and it covers the common case without
	// anybody having to know to pass a flag.
	if name, err := os.Hostname(); err == nil && name != "" {
		hosts = append(hosts, name)
	}
	tlsConfig, err := authority.ServerTLS(hosts)
	if err != nil {
		ln.Close()
		return err
	}
	srv.TLSConfig = tlsConfig

	serveErr := make(chan error, 1) // buffered: the goroutine must not block if we already returned
	go func() {
		// Empty paths: the certificate is in TLSConfig, issued by this process
		// from its own authority, and never touches the filesystem.
		err := srv.ServeTLS(ln, "", "")
		if errors.Is(err, http.ErrServerClosed) {
			err = nil // an expected shutdown is not a failure
		}
		serveErr <- err
	}()

	cfg.Logger.Info("listening", "addr", ln.Addr().String(),
		"transport", "https", "client_certs", "verified if presented")
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

// publishBootstrap writes what a worker needs to enroll itself: the authority to
// verify this control plane, and a token that says it may (D25).
func publishBootstrap(eng *engine.Engine, cfg Config) error {
	authority, err := eng.Authority()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.Layout.BootstrapDir(), 0o755); err != nil {
		return err
	}
	// 0644: a certificate is what clients check against, not a secret.
	if err := os.WriteFile(cfg.Layout.BootstrapCA(), authority.CertPEM(), 0o644); err != nil {
		return err
	}

	token, err := eng.BootstrapToken()
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.Layout.BootstrapToken(), []byte(token+"\n"), 0o600)
}
