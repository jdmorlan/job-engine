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
	}); err != nil {
		ln.Close()
		return err
	}

	srv := &http.Server{
		Handler:           api.New(eng, cfg.Logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second, // cheap protection against a stuck client
	}

	serveErr := make(chan error, 1) // buffered: the goroutine must not block if we already returned
	go func() {
		err := srv.Serve(ln)
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
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Shutdown timed out with requests still in flight. Say so, and
			// carry on shutting down -- the deferred engine.Close matters more.
			cfg.Logger.Warn("graceful shutdown timed out", "error", err)
		}
		return nil
	}
}
