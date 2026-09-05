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

	// Style decides what the output is allowed to look like. Resolved once,
	// from the real stdout, so that a command never has to ask whether it is
	// talking to a terminal -- it asks for a heading and gets one either way.
	Style Style

	// ErrStyle is the same for stderr, which is a different stream and often a
	// different destination: `je runs > file` still has a terminal to complain
	// to, and an error that arrives in plain text while the table it replaced
	// would have been coloured is a worse error, not a safer one.
	ErrStyle Style
}

// Command is one `je` subcommand.
type Command struct {
	Name  string
	Usage string // one-line summary, shown in `je help`
	Args  string // argument shape, e.g. "<type>", shown in the command's own usage
	Long  string // optional detail, shown by `je help <command>`

	// Local marks a command that acts on THIS MACHINE rather than on the
	// control plane wherever it is.
	//
	// The distinction is invisible until it bites, and then it bites hard: `je
	// runs` works identically against a control plane in a cluster, and `je
	// control-plane remove` does not -- it removes a service here, and a
	// control plane in Kubernetes carries on. Somebody with a split deployment
	// finds that out by watching a command succeed and change nothing.
	//
	// It is also a to-do list. Every command marked Local is a place where a
	// deployment split across machines has no answer yet: D19's R2 says every
	// command you learned locally should work against a remote engine by
	// switching context, and these are the ones that do not.
	Local bool

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

// errReported means the command failed and has already said how.
//
// The dispatcher's job is to make sure a failure is never silent, and its
// default way of doing that -- printing "je <cmd>: <err>" -- is exactly wrong
// for a command whose whole output is a rendered report of the failure. Adding
// a second line under a command's own rendered failure would be noise, and dropping
// the non-zero exit would be a lie.
var errReported = reportedError{}

type reportedError struct{}

func (reportedError) Error() string { return "already reported" }

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
	color := global.String("color", "auto", "colourise output: auto, always or never")
	global.Usage = func() { printRootUsage(env.Stderr, global, env.Style) }

	if err := global.Parse(args); err != nil {
		return ExitUsage // flag already printed the problem and the usage
	}

	switch colorMode(*color) {
	case colorAuto, colorAlways, colorNever:
		env.Style = resolveStyle(env.Stdout, colorMode(*color), os.Getenv)
		env.ErrStyle = resolveStyle(env.Stderr, colorMode(*color), os.Getenv)
	default:
		fmt.Fprintf(env.Stderr, "je: --color must be auto, always or never, not %q\n", *color)
		return ExitUsage
	}

	layout, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(env.Stderr, "je: %v\n", err)
		return ExitError
	}
	if *dataDir != "" {
		if layout, err = paths.At(*dataDir); err != nil {
			fmt.Fprintf(env.Stderr, "je: %v\n", err)
			return ExitError
		}
	}
	env.Layout = layout

	rest := global.Args()
	if len(rest) == 0 {
		printRootUsage(env.Stdout, global, env.Style)
		return ExitUsage
	}

	name, rest := rest[0], rest[1:]
	if name == "help" {
		return runHelp(env, global, rest)
	}

	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(env.Stderr, "%s unknown command %q\nRun %s for the list.\n",
			env.ErrStyle.Bad("je:"), name, env.ErrStyle.Cmd("je help"))
		return ExitUsage
	}

	switch err := cmd.Run(ctx, env, rest); {
	case err == nil:
		return ExitOK
	case errors.Is(err, context.Canceled):
		// Ctrl-C. The user knows what happened; do not lecture them about it.
		return ExitError
	default:
		var re reportedError
		if errors.As(err, &re) {
			return ExitError
		}
		var ae attentionError
		if errors.As(err, &ae) {
			// Not printed: the command has already rendered what needs
			// attention, and a trailing error line would just be noise.
			return ExitAttention
		}
		fmt.Fprintf(env.Stderr, "%s %v\n", env.ErrStyle.Bad("je "+cmd.Name+":"), err)
		var ue usageError
		if errors.As(err, &ue) {
			return ExitUsage
		}
		return ExitError
	}
}

func runHelp(env *Env, global *flag.FlagSet, args []string) int {
	if len(args) == 0 {
		printRootUsage(env.Stdout, global, env.Style)
		return ExitOK
	}
	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(env.Stderr, "je: unknown command %q\n", args[0])
		return ExitUsage
	}
	st := env.Style
	fmt.Fprintf(env.Stdout, "usage: %s\n\n%s\n", usageLine(cmd, st), cmd.Usage)
	if cmd.Long != "" {
		fmt.Fprintf(env.Stdout, "\n%s\n", cmd.Long)
	}
	if cmd.Local {
		fmt.Fprint(env.Stdout, "\n"+st.Warn(localScopeTitle)+"\n"+st.Muted(localScopeNote))
	}
	return ExitOK
}

// group is a section of `je help`.
//
// Thirty commands in one alphabetical list is a wall: it is complete, it is
// sorted, and it answers no question anybody arrives with. "chain" and
// "chains" sit next to "control-plane" because of a letter they share, while
// the three commands somebody actually needs on their first day are scattered
// through it.
//
// The groups are ordered by when you meet them, not by importance -- write a
// job, look at what happened, make something happen, configure it, run the
// engine itself. Alphabetical order survives inside each group, where the list
// is short enough for it to be a help rather than a shrug.
type group struct {
	title string
	names []string
}

var groups = []group{
	{"start here", []string{"up", "down"}},
	{"write jobs", []string{"init", "new", "dev", "demo"}},
	{"see what happened", []string{
		"status", "jobs", "runs", "logs", "chains", "chain",
		"events", "state", "waiting", "explain", "workers",
	}},
	{"make something happen", []string{"run", "retry", "emit", "sync", "retention"}},
	{"definitions and secrets", []string{"source", "secret", "enroll", "identity"}},
	{"run the engine yourself", []string{
		"control-plane", "worker", "web", "upgrade", "reset", "version",
	}},
}

func printRootUsage(w io.Writer, global *flag.FlagSet, st Style) {
	fmt.Fprint(w, st.Title("je")+" — a job engine you can debug\n\n")
	fmt.Fprint(w, "usage: je [global flags] "+st.Cmd("<command>")+" [flags]\n")

	// Anything not placed in a group still has to appear: a command that is
	// registered and undocumented is worse than an ugly help screen, and this
	// is what catches the one somebody adds next year.
	placed := map[string]bool{}
	for _, g := range groups {
		for _, n := range g.names {
			placed[n] = true
		}
	}
	var ungrouped []string
	for name := range commands {
		if !placed[name] {
			ungrouped = append(ungrouped, name)
		}
	}
	sort.Strings(ungrouped)

	// One table across every section rather than one per section: the usage
	// column should line up down the whole screen, or the sections read as
	// five small tables that nearly agree with each other.
	local := false
	tw := newTable(w)
	section := func(title string, names []string) {
		var rows []string
		for _, name := range names {
			if _, ok := commands[name]; ok {
				rows = append(rows, name)
			}
		}
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(tw, "\n%s\n", st.Header(title))
		for _, name := range rows {
			cmd := commands[name]
			mark := ""
			if cmd.Local {
				mark, local = " "+st.Muted("*"), true
			}
			fmt.Fprintf(tw, "  %s\t%s%s\n", st.Cmd(name), cmd.Usage, mark)
		}
	}
	for _, g := range groups {
		section(g.title, g.names)
	}
	section("other", ungrouped)
	tw.Flush()

	if local {
		fmt.Fprint(w, "\n"+st.Muted(
			"* acts on THIS MACHINE, not on the control plane wherever it is.\n"+
				"  Everything else works the same against a control plane in a cluster.")+"\n")
	}

	fmt.Fprint(w, "\n"+st.Header("global flags")+"\n")
	global.SetOutput(w)
	global.PrintDefaults()
	fmt.Fprint(w, "\nRun "+st.Cmd("je help <command>")+" for detail.\n")
}

// usageLine is the "usage: je runs [job]" line, with the argument shape held
// back a shade: it is a placeholder, not something to type literally.
//
// A command that takes no arguments has no shape, and gluing an empty one on
// leaves a trailing space that only shows up when somebody copies the line.
func usageLine(cmd *Command, st Style) string {
	line := st.Cmd("je " + cmd.Name)
	if cmd.Args != "" {
		line += " " + st.Muted(cmd.Args)
	}
	return line
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
		fmt.Fprintf(env.Stderr, "usage: %s\n\n%s\n",
			usageLine(cmd, env.ErrStyle), cmd.Usage)
		fs.PrintDefaults()
	}
	return fs
}

// truncate shortens a string for a table cell, marking that it was cut.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// localScopeNote is the sentence every machine-scoped command ends with.
//
// One wording in one place, because the point is that somebody learns the
// distinction once. It is deliberately specific about the failure it prevents:
// the danger is not that these commands error on a split deployment, it is
// that they succeed and change something other than what was meant.
const localScopeTitle = "THIS MACHINE ONLY."

const localScopeNote = `This acts on processes and files here -- not on the control plane if it is
somewhere else. Against a split deployment (a control plane in a cluster, a
worker on your laptop) it will do its work here and leave the rest untouched,
which is rarely what you meant.

Commands that read or change the engine itself -- jobs, runs, secrets, sources,
enrollment -- go to the control plane wherever it is, and are unaffected.
`
