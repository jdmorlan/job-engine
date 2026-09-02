// Command je is the job engine: a single static binary that runs your
// scheduled and event-driven work, and can always answer what ran, why it ran,
// and what it actually processed.
//
// The binary is deliberately three lines of logic. Everything is in
// internal/cli, which makes the whole command surface testable without
// spawning a process, and keeps this file from becoming the place where
// behaviour quietly accumulates.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jdmorlan/job-engine/internal/cli"

	// Embed the IANA timezone database in the binary.
	//
	// D9 schedules carry an explicit IANA timezone, and D19 names "why did my
	// 3am job run at 8pm" as a real bug: a container has no /usr/share/zoneinfo,
	// so time.LoadLocation fails and the schedule silently falls back to UTC.
	// Half a megabyte to make the same binary behave identically on a laptop
	// and in a scratch image is an easy trade -- and it keeps R1's promise that
	// a definition which works locally runs in the cluster unmodified.
	_ "time/tzdata"
)

// version is overridden at build time with -ldflags "-X main.version=v1.2.0".
// Unset builds report "dev" plus the VCS revision the go tool stamps in.
var version = "dev"

func main() {
	// Signals are the process's business, not the engine's, so they are
	// handled here and turned into a context. The daemon and every command
	// see only cancellation, which is what makes them testable.
	//
	// NotifyContext restores the default handler after the first signal, so a
	// second Ctrl-C during a slow shutdown kills the process outright rather
	// than being swallowed. That is the behaviour you want at 2am.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], &cli.Env{Version: version})
	stop() // explicit, because os.Exit does not run deferred functions
	os.Exit(code)
}
