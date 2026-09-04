// Package jobdef parses, validates, and hashes job definitions.
//
// Two rules from the proposal shape this package:
//
// P3 -- files hold intent, the tool renders truth. A job file contains only
// what the author decided; everything else is a default living here. So a
// Definition carries both the effective value of every field and a record of
// which ones were actually written down, because `je explain` has to be able to
// say "1h (default)" versus "1h (jobs/x.yaml:7)".
//
// D19 R1 -- no Kubernetes-shaped fields in job YAML, ever. A definition that
// works locally must run in a cluster unmodified, which is a constraint on this
// schema more than on any deployment tooling. Nothing here may describe where
// the job runs, only what it is.
package jobdef

import (
	"errors"
	"fmt"
	"github.com/jdmorlan/job-engine/internal/toolchain"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Definition is one job, with defaults applied.
//
// The zero value is not valid; definitions come from Parse.
type Definition struct {
	// Slug is the job's identity, taken from the filename rather than the
	// file's contents. Same rule as chains (D17), and it means renaming a job
	// is a file rename -- visible in git, and impossible to do by accident.
	Slug string `json:"slug"`

	DisplayName string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`

	Command []string `json:"command"`
	Workdir string   `json:"workdir,omitempty"`
	Runtime Runtime  `json:"runtime"`

	// Language is the ecosystem this job's code belongs to, and it is what the
	// worker prepares before running the command: dependencies installed from
	// the tree's lockfile, and the right binaries on PATH (D28).
	//
	// Empty means the tree needs no preparation -- a shell script, or anything
	// whose dependencies are already wherever it expects them.
	//
	// Named `language:` rather than `runtime:` because `runtime:` was already
	// taken by the process/container choice (D20), and because a job written in
	// TypeScript should say so once. D21's shim injection keys off the same
	// field when it lands: preparing a tree and injecting helpers are two
	// capabilities of one fact about the job, not two fields to keep in step.
	Language string `json:"language,omitempty"`

	// RunsOn is the capability label a worker must advertise to run this job
	// (D20/C3). Jobs are pinned, not placed: this names what the job needs,
	// not where the deployment happens to put things, which is why it stays
	// true on a laptop and in a cluster alike (D19/R1).
	//
	// The default exists so the first job somebody writes does not have to
	// know that placement is a concept -- a control plane ships with a worker
	// advertising it (C12).
	RunsOn string `json:"runs_on"`

	Timeout     Duration    `json:"timeout"`
	Overlap     Overlap     `json:"overlap"`
	OnInterrupt OnInterrupt `json:"on_interrupt"`
	Enabled     bool        `json:"enabled"`

	Schedules []Schedule `json:"on,omitempty"`
	State     StateSpec  `json:"state"`
	Secrets   []string   `json:"secrets,omitempty"`

	// declared maps a top-level field name to the line it appears on, for
	// `je explain`. Not part of the definition's identity, so it is excluded
	// from the hash and from the stored snapshot.
	declared map[string]int
	filePath string
}

// Runtime selects an executor (D1).
type Runtime string

const (
	RuntimeProcess   Runtime = "process"
	RuntimeContainer Runtime = "container"
)

// Overlap decides what happens when a job is triggered while already running (D8).
type Overlap string

const (
	OverlapSkip  Overlap = "skip"
	OverlapQueue Overlap = "queue"
	OverlapAllow Overlap = "allow"
)

// OnInterrupt decides what happens to a run the engine was killed in the
// middle of (D5). Default fail, because the engine cannot know whether the job
// is safe to repeat -- that is the author's contract to declare.
type OnInterrupt string

const (
	OnInterruptFail   OnInterrupt = "fail"
	OnInterruptRetry  OnInterrupt = "retry"
	OnInterruptIgnore OnInterrupt = "ignore"
)

// CatchUp decides what to do about windows missed while the engine was down (D9).
type CatchUp string

const (
	CatchUpSkip CatchUp = "skip"
	CatchUpOnce CatchUp = "once"
	CatchUpAll  CatchUp = "all"
)

// StateCommit decides when the cursor moves (D14).
type StateCommit string

const (
	// CommitOnSuccess is the whole point of D14: the cursor advances only when
	// the work actually succeeded, so a failure cannot silently skip records.
	CommitOnSuccess StateCommit = "on_success"
	CommitAlways    StateCommit = "always"
)

// Schedule is a job's own clock. D17's rule: a job declares when it wants to
// run on its own, never who it depends on -- inter-job wiring lives in chains.
type Schedule struct {
	Every    Duration `json:"every,omitempty"`
	Cron     string   `json:"cron,omitempty"`
	Timezone string   `json:"timezone,omitempty"`
	CatchUp  CatchUp  `json:"catch_up"`
}

// StateSpec configures the job's cursor (D14).
type StateSpec struct {
	// PrimaryCursor names the key the tool shows in status views, and the key
	// the engine seeds on a first run. State itself stays opaque JSON.
	PrimaryCursor string      `json:"primary_cursor"`
	Commit        StateCommit `json:"commit"`
}

// Defaults, in one place so `je explain` and the docs cannot disagree with the
// engine. Every one of these is a value that does NOT appear in a job file.
const (
	DefaultTimeout       = time.Hour
	DefaultRuntime       = RuntimeProcess
	DefaultOverlap       = OverlapSkip
	DefaultOnInterrupt   = OnInterruptFail
	DefaultCatchUp       = CatchUpSkip
	DefaultStateCommit   = CommitOnSuccess
	DefaultPrimaryCursor = "since"

	// DefaultRunsOn is the label a job needs when it does not say (D20/C3).
	// It must match store.DefaultLabel and the label the system worker
	// advertises; the demo jobs and `je quickstart` both rely on it.
	DefaultRunsOn = "default"
)

// MaxStateBytes caps the cursor at 64KB.
//
// This is a real limit, not a round number: state is delivered in JE_STATE, and
// Linux caps a single environment variable at 128KB (MAX_ARG_STRLEN) while
// macOS caps the whole environment plus arguments at 1MB. 64KB leaves room for
// the rest of the environment on both.
const MaxStateBytes = 64 * 1024

// slugPattern is what a job may be called. Deliberately narrow: the slug ends
// up in file names, CLI arguments, event payloads, and URLs.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ValidName reports whether a string may name a job or a chain.
//
// Exported so `je new` can refuse a bad name while writing the file, rather
// than writing it and letting the next sync reject it.
func ValidName(name string) bool { return slugPattern.MatchString(name) }

// SlugFromPath derives a job's identity from its file name.
func SlugFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// CommandLine renders the command the way you would type it, quoting only what
// needs it. Display only -- nothing executes this string, and the command is
// always an argv (D6), never a shell line.
func (d *Definition) CommandLine() string {
	parts := make([]string, 0, len(d.Command))
	for _, a := range d.Command {
		if a == "" || strings.ContainsAny(a, " \t\"'\\$") {
			parts = append(parts, fmt.Sprintf("%q", a))
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// FilePath reports where this definition was read from.
func (d *Definition) FilePath() string { return d.filePath }

// DeclaredAt returns the line a field was written on, and whether it was
// written at all. This is what lets `je explain` attribute every effective
// value to either the file or a default (P3).
func (d *Definition) DeclaredAt(field string) (int, bool) {
	line, ok := d.declared[field]
	return line, ok
}

// DeclaredLines returns every field the author actually wrote, and the line it
// is on.
//
// It is stored beside the definition rather than inside it: the snapshot is
// hashed (D11), and a line number describes the file rather than the job, so
// adding a comment at the top of a file must not read as a new definition.
func (d *Definition) DeclaredLines() map[string]int {
	out := make(map[string]int, len(d.declared))
	for field, line := range d.declared {
		out[field] = line
	}
	return out
}

// WithDeclaredLines restores what the file said, for a definition rebuilt from
// its stored snapshot.
func (d *Definition) WithDeclaredLines(lines map[string]int) *Definition {
	d.declared = lines
	return d
}

// Validate reports the first thing wrong with a definition.
//
// Errors here are *load-time* errors, which is the point: D10 establishes that
// a job which cannot run should be visibly misconfigured the moment its file is
// saved, not discovered from a cryptic exit code eight hours later.
func (d *Definition) Validate() error {
	if !slugPattern.MatchString(d.Slug) {
		return fmt.Errorf("job name %q must be lowercase letters, digits and dashes", d.Slug)
	}
	if len(d.Command) == 0 {
		return errors.New("command is required")
	}
	for i, arg := range d.Command {
		if arg == "" {
			return fmt.Errorf("command[%d] is empty", i)
		}
	}
	switch d.Runtime {
	case RuntimeProcess:
	case RuntimeContainer:
		return errors.New("runtime: container is not implemented yet (D1)")
	default:
		return fmt.Errorf("runtime must be process or container, got %q", d.Runtime)
	}
	switch d.Overlap {
	case OverlapSkip, OverlapQueue, OverlapAllow:
	default:
		return fmt.Errorf("overlap must be skip, queue or allow, got %q", d.Overlap)
	}
	switch d.OnInterrupt {
	case OnInterruptFail, OnInterruptRetry, OnInterruptIgnore:
	default:
		return fmt.Errorf("on_interrupt must be fail, retry or ignore, got %q", d.OnInterrupt)
	}
	switch d.State.Commit {
	case CommitOnSuccess:
	case CommitAlways:
		return errors.New("state.commit: always is not implemented yet (D14)")
	default:
		return fmt.Errorf("state.commit must be on_success or always, got %q", d.State.Commit)
	}
	if d.Timeout.D <= 0 {
		return fmt.Errorf("timeout must be positive, got %s", d.Timeout)
	}
	if d.State.PrimaryCursor == "" {
		return errors.New("state.primary_cursor cannot be empty")
	}
	for i, s := range d.Schedules {
		if err := s.validate(); err != nil {
			return fmt.Errorf("on[%d]: %w", i, err)
		}
		// These two policies contradict each other, and the contradiction is
		// silent rather than loud: `all` queues every missed window, and `skip`
		// then drops all but the first, so `all` quietly behaves like `once`.
		// Since `skip` is the default overlap, an author writing `catch_up:
		// all` gets the opposite of what they asked for and no indication.
		//
		// Said at load time, where it can still be fixed (D10's pit of
		// success), rather than discovered from a thin timeline at 3am.
		if s.CatchUp == CatchUpAll && d.Overlap == OverlapSkip {
			return fmt.Errorf(
				"on[%d]: catch_up: all needs overlap: queue -- with overlap: skip "+
					"(the default) only the first missed window would run", i)
		}
	}
	return nil
}

func (s Schedule) validate() error {
	switch {
	case s.Every.D > 0 && s.Cron != "":
		return errors.New("set either every or cron, not both")
	case s.Every.D == 0 && s.Cron == "":
		return errors.New("needs either every or cron")
	case s.Every.D > 0 && s.Every.D < time.Second:
		return fmt.Errorf("every must be at least 1s, got %s", s.Every)
	}
	switch s.CatchUp {
	case CatchUpSkip, CatchUpOnce, CatchUpAll:
	default:
		return fmt.Errorf("catch_up must be skip, once or all, got %q", s.CatchUp)
	}
	if s.Timezone != "" {
		if _, err := time.LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("unknown timezone %q", s.Timezone)
		}
	}
	return nil
}

// ConfigError reports a reason this job cannot be scheduled even though its
// file parsed correctly. It populates jobs.config_error, so the job is listed
// and visibly broken rather than silently absent (D10's pit of success).
//
// It covers only what the definition itself can know. A declared secret that
// is not set is also a config error, but this package cannot see the secret
// store, so the engine adds that check at load time (D10).
//
// Returning "" means the job is runnable.
func (d *Definition) ConfigError() string {
	if d.Language != "" {
		if _, ok := toolchain.Lookup(d.Language); !ok {
			return toolchain.Unknown(d.Language).Error()
		}
	}
	return ""
}
