// Package cli is the `je` command-line client.
//
// D15 makes the CLI a thin client with no privileged access: it talks to the
// daemon's HTTP API and has no direct database path, except the deliberate
// `je db` escape hatch for when the daemon is down. Keeping that boundary
// honest is what lets a web UI later be a second client rather than a rewrite.
//
// Dispatch is hand-rolled on the standard flag package rather than a CLI
// framework. The whole mechanism is the hundred lines below, it has no
// dependency to keep current, and the resulting command definitions are plain
// structs. If subcommand nesting or shell completion ever justify the
// dependency, the Command type is close enough to Cobra's shape that swapping
// is mechanical.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/jdmorlan/job-engine/internal/paths"
)

// Exit codes. These are part of the interface: P1 asks that "does anything
// need my attention?" be a query with an exit code rather than a vibe, which
// is what makes the engine scriptable in a way a UI is not.
const (
	ExitOK        = 0
	ExitError     = 1
	ExitUsage     = 2
	ExitAttention = 3 // reserved for `je status --attention` (D12)
)

// Env is everything a command is allowed to touch from the outside world.
//
// Commands take an *Env rather than reaching for os.Stdout or os.Getenv
// directly, so that a test can run any command with buffers and a temporary
// data directory and assert on the exact bytes it produced.
type Env struct {
	Stdout  io.Writer
	Stderr  io.Writer
	Stdin   io.Reader
	Layout  paths.Layout
	Version string
}

// Command is one `je` subcommand.
type Command struct {
	Name  string
	Usage string // one-line summary, shown in `je help`
	Args  string // argument shape, e.g. "<type>", shown in the command's own usage
	Long  string // optional detail, shown by `je help <command>`

	// Run does the work. Returning an error prints it and exits non-zero; the
	// command itself never calls os.Exit, so it stays testable.
	Run func(ctx context.Context, env *Env, args []string) error
}

// errAttention means the command succeeded and found something a human should
// look at. P1 asks that "does anything need my attention?" be a query with an
// exit code, which is what makes it scriptable in a way a dashboard is not.
var errAttention = attentionError{}

type attentionError struct{}

func (attentionError) Error() string { return "something needs your attention" }

// usageError signals a mistake in how the command was invoked, as opposed to a
// failure while doing what was asked. It exits 2 so scripts can tell the two
// apart.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// commands is the registry, keyed by name.
var commands = map[string]*Command{}

func register(c *Command) { commands[c.Name] = c }

// Main runs one `je` invocation and returns the process exit code.
//
// It is separate from func main so that the binary's entry point is three
// lines and everything interesting is testable without spawning a process.
func Main(ctx context.Context, args []string, env *Env) int {
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}

	// Global flags come before the subcommand. flag.Parse stops at the first
	// non-flag argument, which gives us `je --data-dir X status --attention`
	// with the subcommand's flags left untouched for the subcommand to parse.
	global := flag.NewFlagSet("je", flag.ContinueOnError)
	global.SetOutput(env.Stderr)
	dataDir := global.String("data-dir", "", "override the engine data directory")
	global.Usage = func() { printRootUsage(env.Stderr, global) }

	if err := global.Parse(args); err != nil {
		return ExitUsage // flag already printed the problem and the usage
	}

	layout, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(env.Stderr, "je: %v\n", err)
		return ExitError
	}
	if *dataDir != "" {
		layout.Data = *dataDir
		layout.Jobs = layout.Data + "/jobs"
	}
	env.Layout = layout

	rest := global.Args()
	if len(rest) == 0 {
		printRootUsage(env.Stdout, global)
		return ExitUsage
	}

	name, rest := rest[0], rest[1:]
	if name == "help" {
		return runHelp(env, global, rest)
	}

	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(env.Stderr, "je: unknown command %q\nRun 'je help' for the list.\n", name)
		return ExitUsage
	}

	switch err := cmd.Run(ctx, env, rest); {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled):
		// Ctrl-C. The user knows what happened; do not lecture them about it.
		return ExitError
	default:
		var ae attentionError
		if errors.As(err, &ae) {
			// Not printed: the command has already rendered what needs
			// attention, and a trailing error line would just be noise.
			return ExitAttention
		}
		fmt.Fprintf(env.Stderr, "je %s: %v\n", cmd.Name, err)
		var ue usageError
		if errors.As(err, &ue) {
			return ExitUsage
		}
		return ExitError
	}
}

func runHelp(env *Env, global *flag.FlagSet, args []string) int {
	if len(args) == 0 {
		printRootUsage(env.Stdout, global)
		return ExitOK
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(env.Stderr, "je: unknown command %q\n", args[0])
		return ExitUsage
	}
	fmt.Fprintf(env.Stdout, "usage: je %s %s\n\n%s\n", cmd.Name, cmd.Args, cmd.Usage)
	if cmd.Long != "" {
		fmt.Fprintf(env.Stdout, "\n%s\n", cmd.Long)
	}
	return ExitOK
}

func printRootUsage(w io.Writer, global *flag.FlagSet) {
	fmt.Fprint(w, "je — a job engine you can debug\n\nusage: je [global flags] <command> [flags]\n\ncommands:\n")

	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, name := range names {
		fmt.Fprintf(tw, "  %s\t%s\n", name, commands[name].Usage)
	}
	tw.Flush()

	fmt.Fprint(w, "\nglobal flags:\n")
	global.SetOutput(w)
	global.PrintDefaults()
	fmt.Fprint(w, "\nRun 'je help <command>' for detail.\n")
}

// parseArgs parses a subcommand's flags and returns its positional arguments,
// allowing the two to be interleaved.
//
// The standard flag package stops parsing at the first non-flag argument, so
// `je emit homekit.motion --payload '{}'` would otherwise treat --payload as a
// positional and silently ignore it. Requiring flags to come first is the
// usual workaround and it is a bad one: every other tool on the machine
// accepts either order, and "the flag was ignored" is exactly the kind of
// quiet wrongness this project exists to avoid.
//
// The loop below is the whole fix: parse, take one positional, parse what is
// left, repeat. It handles flags with separate values correctly because each
// pass is a real flag.Parse rather than a hand-rolled scan.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err}
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// newFlagSet builds a flag set for a subcommand, wired to the env's stderr and
// to the command's own usage line.
func newFlagSet(cmd *Command, env *Env) *flag.FlagSet {
	fs := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(env.Stderr, "usage: je %s %s\n\n%s\n", cmd.Name, cmd.Args, cmd.Usage)
		fs.PrintDefaults()
	}
	return fs
}
