package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/executor"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/shim"
	"github.com/jdmorlan/job-engine/internal/toolchain"
)

func init() {
	register(&Command{
		Name:  "try",
		Args:  "<job>",
		Usage: "run a job here from its file, without a control plane",
		Long: "For writing jobs. It reads the definition out of this directory, prepares\n" +
			"the tree exactly as a worker would -- dependencies from your lockfile, and\n" +
			"the helpers your language imports -- runs the command, and shows what the\n" +
			"engine WOULD have committed.\n\n" +
			"Nothing is recorded. There is no run, no history, no cursor movement and no\n" +
			"control plane involved, so this is safe to type in a loop while you get a\n" +
			"job working. `je run` is the real thing.\n\n" +
			"The checks are the engine's own: the same size caps, the same JSON rules,\n" +
			"the same event parsing. A harness that enforced something slightly\n" +
			"different would be worse than none, because it would send you to\n" +
			"production confident about a contract nobody checks that way.\n\n" +
			"  je try ingest\n" +
			"  je try ingest --state '{\"since\":\"2026-09-01T00:00:00Z\"}'\n" +
			"  je try ingest --event '{\"path\":\"/tmp/new.csv\"}'",
		// Deliberately NOT marked Local, although it only ever acts on this
		// machine. That flag means "this is a gap for a split deployment" --
		// the marked list is a to-do list (D26) -- and running here is not a
		// limitation of this command, it is the entire point of it. Marking it
		// would print a note warning you about the behaviour you asked for.
		Run: runTry,
	})
}

func runTry(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["try"], env)
	var (
		state   = fs.String("state", "", "the cursor to start from, as JSON (default: empty)")
		event   = fs.String("event", "", "the triggering event's payload, as JSON")
		dir     = fs.String("dir", ".", "the repository holding the job definition")
		keep    = fs.Bool("keep", false, "keep the scratch directory and print where it is")
		timeout = fs.Duration("timeout", 0, "override the job's own timeout")
	)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("expected exactly one job name, got %d", len(positional))
	}

	def, jobDir, err := loadLocalDefinition(*dir, positional[0])
	if err != nil {
		return err
	}
	// Both inputs are checked before anything runs, because a typo in a flag
	// should not cost you a job execution to discover.
	stateIn, err := jsonObjectFlag(*state, "state")
	if err != nil {
		return err
	}
	eventIn, err := jsonObjectFlag(*event, "event")
	if err != nil {
		return err
	}

	tree, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	// The tree is the repository -- dependencies and the helpers are prepared
	// there, once, for every job in it. The job's own folder is only where its
	// command runs.
	workdir, err := filepath.Abs(jobDir)
	if err != nil {
		return err
	}
	return tryJob(ctx, env, def, tree, tryOptions{
		Workdir: workdir,
		State:   stateIn, Event: eventIn, Keep: *keep, Timeout: *timeout,
	})
}

type tryOptions struct {
	// Workdir is where the command runs: the job's own folder, which for a
	// flat job is the repository itself.
	Workdir string

	State   json.RawMessage
	Event   json.RawMessage
	Keep    bool
	Timeout time.Duration
}

// tryJob is the whole harness: prepare, run, check.
func tryJob(ctx context.Context, env *Env, def *jobdef.Definition, tree string, opts tryOptions) error {
	// The same two steps a worker takes before a command, in the same order
	// and through the same code (D28, D21). This is most of the value: "does
	// my language get set up correctly?" is the question, and answering it
	// with a reimplementation would answer a different one.
	if def.Language != "" {
		fmt.Fprintf(env.Stderr, "je: preparing %s in %s\n", def.Language, tree)
	}
	binDir, err := prepareLocally(ctx, env, def.Language, tree)
	if err != nil {
		return err
	}
	shimEnv, err := shim.Install(def.Language, tree)
	if err != nil {
		return err
	}

	scratch, err := os.MkdirTemp("", "je-try-"+strings.ReplaceAll(def.Slug, "/", "-")+"-")
	if err != nil {
		return err
	}
	if opts.Keep {
		fmt.Fprintf(env.Stderr, "je: scratch directory %s\n", scratch)
	} else {
		defer os.RemoveAll(scratch)
	}

	stateOut := filepath.Join(scratch, "state.json")
	output := filepath.Join(scratch, "output.json")
	events := filepath.Join(scratch, "events.jsonl")

	// D6's environment, built here rather than borrowed, because the control
	// plane's version of it carries a run id and a cursor version that do not
	// exist for something that is not a run. What a job can READ is identical.
	jobEnv := append([]string{
		"JOB_ID=" + def.Slug,
		"RUN_ID=0",
		"ATTEMPT=1",
		"TRIGGERED_BY=try",
		"EVENT_PAYLOAD=" + string(orEmptyObject(opts.Event)),
		"JE_STATE=" + string(orEmptyObject(opts.State)),
		"JOB_WORKDIR=" + scratch,
		"JOB_STATE_OUT_FILE=" + stateOut,
		"JOB_OUTPUT_FILE=" + output,
		"JOB_EVENTS_FILE=" + events,
	}, shimEnv...)
	jobEnv = append(jobEnv, passThroughEnv()...)
	if binDir != "" {
		jobEnv = append(jobEnv, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	// Declared secrets are named rather than resolved. There is no secret
	// store to read here and inventing one would be worse than the honest
	// gap: a job that needs credentials is a job to run with `je run`.
	if len(def.Secrets) > 0 {
		fmt.Fprintf(env.Stderr,
			"je: this job declares %s, which `je try` cannot provide -- "+
				"set them in your shell if the job needs them here\n",
			strings.Join(def.Secrets, ", "))
	}

	// What the file said wins over where the file is, the same as it does on a
	// worker: a job declaring `workdir:` meant it.
	workdir := opts.Workdir
	if declared := def.Workdir; declared != "" {
		if filepath.IsAbs(declared) {
			workdir = declared
		} else {
			workdir = filepath.Join(tree, declared)
		}
	}

	limit := def.Timeout.D
	if opts.Timeout > 0 {
		limit = opts.Timeout
	}
	result, execErr := executor.Process{}.Run(ctx, executor.Spec{
		Command: def.Command,
		Workdir: workdir,
		Env:     jobEnv,
		Timeout: limit,
		Grace:   executor.DefaultGrace,
		Output:  terminalSink{env: env},
	})
	if execErr != nil {
		return fmt.Errorf("%s did not start: %w", def.CommandLine(), execErr)
	}

	return reportTry(env, def, result, stateOut, output, events)
}

// reportTry is the part that makes this a check rather than just a way to run
// a command: what the engine would have done with what the job wrote.
func reportTry(env *Env, def *jobdef.Definition, result executor.Result, stateOut, output, events string) error {
	fmt.Fprintln(env.Stdout)
	if !result.Succeeded() {
		fmt.Fprintf(env.Stdout, "x  %s\n", execFailureText(result))
		// The channels are deliberately not checked. A failed run commits
		// nothing (D14), so reporting on what it wrote would describe a
		// promotion that would never happen.
		fmt.Fprintln(env.Stdout, "   nothing would be committed: a run that fails commits nothing")
		return errReported
	}

	state, out, emitted, err := engine.ValidateChannels(
		readOrNil(stateOut), readOrNil(output), readOrNil(events))
	if err != nil {
		// This is the failure mode `je try` exists to catch early: the job
		// exited zero and broke the contract, which in a real run is a failed
		// run with a message you have to go and read.
		fmt.Fprintf(env.Stdout, "x  the job exited 0 but broke the protocol:\n   %v\n", err)
		return errReported
	}

	fmt.Fprintf(env.Stdout, "ok  %s would have succeeded\n", def.Slug)
	cursor := def.State.PrimaryCursor
	switch {
	case state != nil:
		fmt.Fprintf(env.Stdout, "    cursor  would move to %s\n", truncate(string(state), 100))
	default:
		fmt.Fprintf(env.Stdout, "    cursor  unchanged (%s)\n", cursor)
	}
	if out != nil {
		fmt.Fprintf(env.Stdout, "    output  %s\n", truncate(string(out), 100))
	}
	for _, ev := range emitted {
		fmt.Fprintf(env.Stdout, "    emit    %s %s\n", ev.Type, truncate(string(ev.Payload), 80))
	}
	return nil
}

// loadLocalDefinition reads one job file out of a repository on disk.
//
// The file rather than the control plane, deliberately: this command is for the
// version you are editing, which by definition has not been synced anywhere.
func loadLocalDefinition(dir, name string) (*jobdef.Definition, string, error) {
	// An explicit file wins, so that `je try ./some/job.yaml` does what it
	// says while a bare name goes looking.
	if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
		path := filepath.Join(dir, name)
		def, err := parseLocal(path, jobdef.SlugFromPath(name))
		return def, filepath.Dir(path), err
	}

	// Both layouts, in the order somebody would guess: the folder that is the
	// job, then a file at the top level. The same two forms the engine loads,
	// because a harness that could not find what the engine runs would send
	// people to look for a bug in the wrong place.
	candidates := []struct{ path, workdir string }{
		{filepath.Join(dir, name, "job.yaml"), filepath.Join(dir, name)},
		{filepath.Join(dir, name, "job.yml"), filepath.Join(dir, name)},
		{filepath.Join(dir, name+".yaml"), dir},
		{filepath.Join(dir, name+".yml"), dir},
	}
	for _, c := range candidates {
		if _, err := os.Stat(c.path); err != nil {
			continue
		}
		def, err := parseLocal(c.path, name)
		return def, c.workdir, err
	}
	return nil, "", fmt.Errorf(
		"no job called %q in %s.\n"+
			"Looked for %s/job.yaml and %s.yaml. `je try` reads the definition from "+
			"this directory, which is where you are editing it -- use --dir if your "+
			"jobs are somewhere else", name, dir, name, name)
}

func parseLocal(path, slug string) (*jobdef.Definition, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return jobdef.Parse(path, slug, body)
}

// prepareLocally installs a language's dependencies into the tree, the way a
// worker does (D28).
func prepareLocally(ctx context.Context, env *Env, language, tree string) (string, error) {
	if language == "" {
		return "", nil
	}
	tc, ok := toolchain.Lookup(language)
	if !ok {
		return "", toolchain.Unknown(language)
	}
	if _, err := os.Stat(filepath.Join(tree, tc.Manifest)); err != nil {
		// Nothing to install, which is an ordinary job rather than a mistake.
		return "", nil
	}
	cmd := tc.Install
	fmt.Fprintf(env.Stderr, "je: %s\n", strings.Join(cmd, " "))
	result, err := executor.Process{}.Run(ctx, executor.Spec{
		Command: cmd,
		Workdir: tree,
		Env:     passThroughEnv(),
		Timeout: 10 * time.Minute,
		Grace:   executor.DefaultGrace,
		Output:  terminalSink{env: env},
	})
	if err != nil {
		return "", fmt.Errorf("installing %s dependencies: %w", language, err)
	}
	if !result.Succeeded() {
		return "", fmt.Errorf("installing %s dependencies failed", language)
	}
	if tc.BinDir == "" {
		return "", nil
	}
	return filepath.Join(tree, tc.BinDir), nil
}

// terminalSink streams a job's output straight to the terminal.
//
// No sequence numbers and no storage: there is nothing to store it in, and the
// terminal is the whole audience for a command you are running while editing.
type terminalSink struct{ env *Env }

func (s terminalSink) WriteLine(stream executor.Stream, _ time.Time, line string) {
	w := s.env.Stdout
	if stream == executor.StreamStderr {
		w = s.env.Stderr
	}
	fmt.Fprintln(w, line)
}

// passThroughEnv is what a job inherits here.
//
// Deliberately your whole environment, unlike a real run. D10 scrubs the
// environment so a scheduled job cannot inherit credentials by accident; this
// is a command you typed in your own shell to run your own code, and stripping
// PATH out from under it would break every job that uses a tool you installed.
// The difference is worth knowing about, which is why it is documented rather
// than quietly matched.
func passThroughEnv() []string { return os.Environ() }

func jsonObjectFlag(raw, name string) (json.RawMessage, error) {
	if raw == "" {
		return nil, nil
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		return nil, fmt.Errorf("--%s must be a JSON object: %w", name, err)
	}
	return json.RawMessage(raw), nil
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func readOrNil(path string) []byte {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return body
}

func execFailureText(r executor.Result) string {
	switch {
	case r.TimedOut && r.Killed:
		return "timed out, then did not exit within the grace period and was killed"
	case r.TimedOut:
		return "timed out"
	case r.ExitCode != nil:
		return fmt.Sprintf("exited %d", *r.ExitCode)
	default:
		return "did not produce an exit code"
	}
}
