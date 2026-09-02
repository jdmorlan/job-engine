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

	// Language opts the job into shim injection (D21). Empty means the raw
	// protocol, which is the floor every language reaches.
	Language string `json:"language,omitempty"`

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

// SlugFromPath derives a job's identity from its file name.
func SlugFromPath(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
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
// Returning "" means the job is runnable.
func (d *Definition) ConfigError() string {
	if len(d.Secrets) > 0 {
		// Honest placeholder. The field is already the right shape for D10, so
		// no job file needs editing when the secret store lands -- but silently
		// running a job without the credentials it declared would be worse than
		// refusing to run it.
		return "secrets are not implemented yet (D10); this job will not run"
	}
	if d.Language != "" {
		return fmt.Sprintf("language: %s -- shim injection is not implemented yet (D21)", d.Language)
	}
	return ""
}
