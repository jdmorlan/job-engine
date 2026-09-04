package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/schedule"
	"github.com/jdmorlan/job-engine/internal/toolchain"
)

func init() {
	register(&Command{
		Name:  "new",
		Args:  "<name>",
		Usage: "write a new job file, and optionally the script it runs",
		Long: "Writes jobs/<name>.yaml containing only what you asked for. It is a\n" +
			"starting point, not a form: everything the engine will do that is\n" +
			"not in the file is a default, and `je explain <name>` shows every\n" +
			"one of them next to the line that overrode it (P3).\n\n" +
			"That is why this does not write twenty commented-out settings. A\n" +
			"file full of defaults you did not choose is the thing this engine's\n" +
			"job files exist not to be.\n\n" +
			"--language also writes scripts/<name>.<ext> with the whole job\n" +
			"protocol in it: the cursor, the output channel, and events. There is\n" +
			"no SDK to import -- the filesystem is the contract (D6) -- so the\n" +
			"template is the documentation.\n\n" +
			"  je new nightly --language sh          scripts/nightly.sh\n" +
			"  je new ingest --language python       scripts/ingest.py\n" +
			"  je new ingest --language typescript   scripts/ingest.ts, and the\n" +
			"                                       helpers to import (D21)\n\n" +
			"For a language the engine can prepare, it also writes `language:` into\n" +
			"the job file -- which is what makes a worker install your dependencies\n" +
			"from your lockfile and give you the helpers to import (D28, D21).",
		Run: runNew,
	})
}

func runNew(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["new"], env)
	var (
		chain       = fs.Bool("chain", false, "write a chain file instead of a job")
		language    = fs.String("language", "", "also write scripts/<name>.<ext> in this language and point the job at it")
		script      = fs.Bool("script", false, "deprecated alias for --language sh")
		command     = fs.String("command", "", "the command to run, as you would type it")
		description = fs.String("description", "", "one line saying what this is for")
		every       = fs.String("every", "", "run on an interval, e.g. 15m")
		cron        = fs.String("cron", "", "run on a cron schedule, e.g. \"0 3 * * *\"")
		runsOn      = fs.String("runs-on", "", "the worker label this job needs, e.g. macos")
		dir         = fs.String("dir", "", "the jobs repository to write into (default: this one, or the engine's)")
	)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("give exactly one name")
	}
	name := rest[0]

	root, err := jobsRoot(env, *dir)
	if err != nil {
		return err
	}

	lang, err := resolveLanguage(env, *language, *script)
	if err != nil {
		return err
	}

	if *chain {
		return writeChainFile(env, root, name, *description)
	}
	return writeJobFile(env, root, name, jobTemplate{
		description: *description,
		command:     *command,
		every:       *every,
		cron:        *cron,
		runsOn:      *runsOn,
		language:    lang,
	})
}

// jobsRoot decides which repository a new file belongs in.
//
// The rule is the one somebody would guess: the repository you are standing in.
// The path written is always printed, so a wrong guess is visible immediately
// rather than being discovered when the job does not appear.
func jobsRoot(env *Env, override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}

	// The current directory, because that is where a jobs repository is when
	// you are working in one. There is no fallback to somewhere the engine
	// owns: it owns no definitions.
	return os.Getwd()
}

type jobTemplate struct {
	description string
	command     string
	every       string
	cron        string
	runsOn      string
	language    scriptLanguage
}

func writeJobFile(env *Env, root, name string, t jobTemplate) error {
	if err := checkName(name, "job"); err != nil {
		return err
	}
	if t.every != "" && t.cron != "" {
		return usagef("--every and --cron are two ways to say the same thing; pick one")
	}
	// Checked here rather than at load time, because the whole value of writing
	// the file for you is that it is right when you open it.
	if t.cron != "" {
		if _, err := schedule.Parse(schedule.Spec{Cron: t.cron}); err != nil {
			return usagef("--cron %q: %v", t.cron, err)
		}
	}
	if t.every != "" {
		var d jobdef.Duration
		if err := d.UnmarshalJSON([]byte(`"` + t.every + `"`)); err != nil {
			return usagef("--every %q: %v", t.every, err)
		}
	}

	jobPath := filepath.Join(root, name+".yaml")
	scriptRel := filepath.Join("scripts", name+t.language.ext)
	scriptPath := filepath.Join(root, scriptRel)

	if err := refuseToClobber(jobPath); err != nil {
		return err
	}
	if t.language.set() {
		if err := refuseToClobber(scriptPath); err != nil {
			return err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", titleFromSlug(name))
	if t.description != "" {
		fmt.Fprintf(&b, "description: %s\n", t.description)
	}
	b.WriteString("\n")

	// The declaration, for a language the worker knows how to prepare. Written
	// above the command because it is a fact about the job rather than a
	// detail of how it starts, and P3 wants a file to read as decisions.
	if t.language.set() {
		if _, ok := toolchain.Lookup(t.language.name); ok {
			fmt.Fprintf(&b, "language: %s\n\n", t.language.name)
		}
	}

	switch {
	case t.language.set():
		fmt.Fprintf(&b, "command: [%q, %q]\n", t.language.interpreter, scriptRel)
	case t.command != "":
		fmt.Fprintf(&b, "command: %s\n", yamlStringList(strings.Fields(t.command)))
	default:
		// A job with no command does not load, so the placeholder is a working
		// command rather than a TODO: `je run` works the moment this is
		// written, and the first thing you learn is the loop.
		fmt.Fprintf(&b, "command: [\"echo\", \"%s ran\"]\n", name)
	}

	if t.runsOn != "" {
		fmt.Fprintf(&b, "\nruns_on: %s\n", t.runsOn)
	}
	switch {
	case t.every != "":
		fmt.Fprintf(&b, "\non:\n  - every: %s\n", t.every)
	case t.cron != "":
		fmt.Fprintf(&b, "\non:\n  - cron: \"%s\"\n", t.cron)
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("creating jobs directory: %w", err)
	}
	if err := os.WriteFile(jobPath, []byte(b.String()), 0o644); err != nil {
		return err
	}

	// Parsed back before we say it worked. Writing a file the engine will
	// reject, and finding out on the next sync, would be a worse first
	// experience than any error message.
	if _, err := jobdef.Parse(jobPath, name, []byte(b.String())); err != nil {
		return fmt.Errorf("wrote a file the engine will not load, which is a bug: %w", err)
	}

	fmt.Fprintf(env.Stdout, "wrote %s\n", jobPath)
	if t.language.set() {
		if err := os.MkdirAll(filepath.Dir(scriptPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(scriptPath, []byte(t.language.template(name)), 0o755); err != nil {
			return err
		}
		fmt.Fprintf(env.Stdout, "wrote %s\n", scriptPath)

		// Whatever the language needs before a job in it can run at all --
		// which is only TypeScript, and only a package.json declaring tsx.
		// Never overwritten: an existing one is the author's, and this
		// scaffold has no business having an opinion about it.
		for rel, body := range t.language.project {
			path := filepath.Join(root, rel)
			if _, err := os.Stat(path); err == nil {
				continue
			}
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "wrote %s\n", path)
		}
	}

	// Commit and push first, deliberately. The engine reads a repository, so a
	// file that is only on this disk is not a job the engine has -- and
	// `je sync` on its own would be a command that appears to do nothing.
	fmt.Fprintln(env.Stdout)
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	// Anything the author has to do by hand first. It goes above `je try`
	// rather than in the prose, because a step you have to notice is a step
	// somebody will not.
	for _, step := range t.language.next {
		fmt.Fprintf(tw, "  %s\n", step)
	}
	fmt.Fprintf(tw, "  je try %s\trun it here, before anything is committed\n", name)
	fmt.Fprintf(tw, "  git add -A && git commit -m %q && git push\tthe engine reads the repository\n", "add "+name)
	fmt.Fprintf(tw, "  je source sync <source>\tfetch what you just pushed\n")
	fmt.Fprintf(tw, "  je explain <source>/%s\tevery value, including the ones this file does not set\n", name)
	fmt.Fprintf(tw, "  je run <source>/%s\trun it now\n", name)
	return tw.Flush()
}

func writeChainFile(env *Env, root, name, description string) error {
	if err := checkName(name, "chain"); err != nil {
		return err
	}
	path := filepath.Join(root, "chains", name+".yaml")
	if err := refuseToClobber(path); err != nil {
		return err
	}

	if description == "" {
		description = "what this flow is for"
	}
	// Deliberately not loadable as written: a chain naming jobs that do not
	// exist is a load error, and inventing plausible job names would mean the
	// first `je sync` after `je new --chain` fails with an error about jobs the
	// author never mentioned. Commented out, it waits for real names.
	body := fmt.Sprintf(`# One flow per file, and this file's name is the chain's name (D17).
#
# A job never names another job; the wiring lives here, so "what happens after
# X?" is a file you open rather than a grep across every job.
description: %s

steps: []

# Replace the empty list above with real steps. Two kinds of pattern, both the
# same mechanism -- an event a job emitted itself, and an event the engine emits
# about every run:
#
# steps:
#   - on: { event: weather.ingested }
#     run: normalize-readings
#
#   - on: { event: run.succeeded, where: { job: normalize-readings } }
#     run: daily-rollup
#
# `+"`where`"+` compares top-level fields of the event payload for equality, and
# deliberately nothing more.
`, description)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating chains directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "wrote %s\n\n", path)
	fmt.Fprintln(env.Stdout, "it has no steps yet, so it wires nothing. Add some, then:")
	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  je sync\tload it\n")
	fmt.Fprintf(tw, "  je chain %s\twhat it wires, and how the last pass went\n", name)
	return tw.Flush()
}

// scriptTemplate is the whole job protocol, in a file you can read.
//
// D6 says the filesystem is the contract and there is nothing to import, which
// is only an advantage if somebody can find out what the contract is. This is
// where they find out.
func scriptTemplate(name string) string {
	return fmt.Sprintf(`#!/bin/sh
# %s
#
# The engine talks to this script through the environment and three files.
# There is no SDK and nothing to import: the filesystem is the contract (D6),
# and everything below works the same in any language.
set -e

# JE_STATE is your cursor, as JSON. On the very first run the engine seeds it
# with the run's start time, so it is never empty and never the epoch -- there
# is no recovering from having already hammered somebody's API for all history.
echo "cursor in:  $JE_STATE"

# Also set: JOB_ID, RUN_ID, ATTEMPT, TRIGGERED_BY, EVENT_PAYLOAD, JOB_WORKDIR,
# and JE_LAST_SUCCESS_AT (absent if this job has never succeeded).
echo "run $RUN_ID, attempt $ATTEMPT, triggered by $TRIGGERED_BY"

# Anything printed is captured and stored, and is what `+"`je logs`"+` shows.
echo "doing the work"

# Write the new cursor here. Not writing the file means "no change", which is a
# supported outcome rather than an error.
#
# The engine commits this ONLY if this script exits zero. That is the whole
# point of D14: a job that fails half way does not advance its watermark and
# silently skip the records it never processed.
printf '{"since":"%%s"}' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" > "$JOB_STATE_OUT_FILE"

# Structured output, for anything reading this run's result.
printf '{"processed":0}' > "$JOB_OUTPUT_FILE"

# Emit events, one JSON object per line. A chain step can trigger on these, so
# this is how one job sets off another without naming it.
printf '{"type":"%s.finished","payload":{"processed":0}}\n' >> "$JOB_EVENTS_FILE"
`, titleFromSlug(name), name)
}

func refuseToClobber(path string) error {
	if _, err := os.Stat(path); err == nil {
		// Once written, the file is yours. Overwriting it because you reused a
		// name would be the worst thing this command could do.
		return fmt.Errorf("%s already exists", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func checkName(name, what string) error {
	if !jobdef.ValidName(name) {
		return usagef("a %s name must be lowercase letters, digits and dashes: %q", what, name)
	}
	return nil
}

// titleFromSlug turns weather-ingest into "Weather Ingest", so the generated
// file has a display name worth keeping rather than one worth deleting.
func titleFromSlug(slug string) string {
	words := strings.Split(slug, "-")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func yamlStringList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, i := range items {
		quoted = append(quoted, fmt.Sprintf("%q", i))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
