package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jdmorlan/job-engine/internal/jobdef"
)

// Explanation is the whole truth about one job: every effective value, and
// where it came from.
//
// This is the second half of P3. The file holds only what the author decided,
// which is what keeps a job file honest and short -- but it only works if
// something can render the rest, because a reader otherwise cannot tell a
// timeout somebody chose from a default that happens to suit them. A file that
// omits the defaults and a tool that cannot show them is not minimalism, it is
// a gap.
type Explanation struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	FilePath    string `json:"file_path"`

	Fields   []ExplainedField   `json:"fields"`
	Secrets  []ExplainedSecret  `json:"secrets,omitempty"`
	Triggers []ExplainedTrigger `json:"triggers,omitempty"`

	// Problem is why this job cannot run, if it cannot.
	Problem string `json:"problem,omitempty"`
}

// ExplainedField is one effective value and its provenance.
type ExplainedField struct {
	Field string `json:"field"`
	Value string `json:"value"`

	// Line is where the author wrote it, or zero when this is a default.
	// Nested keys cite their parent, since that is the resolution a YAML
	// document gives us and pointing at the right region beats pointing
	// nowhere.
	Line int `json:"line,omitempty"`
}

// Declared reports whether the author wrote this value down.
func (f ExplainedField) Declared() bool { return f.Line > 0 }

// ExplainedSecret is a declared secret and whether it is available (D10).
type ExplainedSecret struct {
	Name string `json:"name"`
	Set  bool   `json:"set"`
}

// ExplainedTrigger is one thing that can start this job.
//
// Both kinds live here on purpose. A job's own schedule is written in its file
// and a chain step is written somewhere else entirely, and "what starts this
// job?" is one question -- answering it with only the half that happens to be
// in front of you is how event-driven systems become unreadable (D17).
type ExplainedTrigger struct {
	Kind string `json:"kind"` // "schedule" or "chain"

	// Schedule is set for a clock: "every 15m", "0 3 * * *".
	Schedule string `json:"schedule,omitempty"`
	CatchUp  string `json:"catch_up,omitempty"`

	// Chain, Step and Match are set for a rule in a chain file.
	Chain string `json:"chain,omitempty"`
	Step  int    `json:"step,omitempty"`
	Match string `json:"match,omitempty"`

	// File is where this trigger is written, which for a chain is not the
	// job's own file.
	File string `json:"file"`
}

// Explain renders every effective value of a job with its provenance.
func (e *Engine) Explain(ctx context.Context, slug string) (Explanation, error) {
	def, job, err := e.Definition(ctx, slug)
	if err != nil {
		return Explanation{}, err
	}

	x := Explanation{
		Slug:        job.Slug,
		DisplayName: def.DisplayName,
		Description: def.Description,
		FilePath:    job.FilePath,
	}
	switch {
	case job.LoadError != "":
		x.Problem = job.LoadError
	case job.ConfigError != "":
		x.Problem = job.ConfigError
	case job.Removed():
		x.Problem = "the definition file is gone; history is kept and it will not run"
	case !job.Enabled:
		x.Problem = "disabled in its file"
	}

	at := func(field string) int {
		line, _ := def.DeclaredAt(field)
		return line
	}
	add := func(field, value string, line int) {
		x.Fields = append(x.Fields, ExplainedField{Field: field, Value: value, Line: line})
	}

	add("command", def.CommandLine(), at("command"))
	if def.Workdir != "" {
		add("workdir", def.Workdir, at("workdir"))
	}
	add("runtime", string(def.Runtime), at("runtime"))
	if def.Language != "" {
		add("language", def.Language, at("language"))
	}
	add("runs_on", def.RunsOn, at("runs_on"))
	add("timeout", def.Timeout.String(), at("timeout"))
	add("overlap", string(def.Overlap), at("overlap"))
	// A reader asking "does this retry?" gets an answer here rather than an
	// absence -- including when the answer is no, which is the default.
	add("retry", retryText(def.Retry), at("retry"))
	add("on_interrupt", string(def.OnInterrupt), at("on_interrupt"))
	add("state.commit", string(def.State.Commit), at("state.commit"))
	add("state.primary_cursor", def.State.PrimaryCursor, at("state.primary_cursor"))

	for _, name := range def.Secrets {
		missing, err := e.secrets.Missing([]string{name})
		if err != nil {
			return Explanation{}, err
		}
		x.Secrets = append(x.Secrets, ExplainedSecret{Name: name, Set: len(missing) == 0})
	}

	for _, s := range def.Schedules {
		t := ExplainedTrigger{
			Kind:     "schedule",
			Schedule: scheduleText(s),
			CatchUp:  string(s.CatchUp),
			File:     job.FilePath,
		}
		x.Triggers = append(x.Triggers, t)
	}

	// The half that is not in this file. A chain step pointing here is a
	// reason this job runs, and it is written in a file the reader is not
	// looking at.
	routes, err := e.store.ActiveRoutes(ctx)
	if err != nil {
		return Explanation{}, err
	}
	for _, r := range routes {
		if r.TargetJobID != job.ID {
			continue
		}
		var match jobdef.Match
		_ = json.Unmarshal(r.Match, &match)
		x.Triggers = append(x.Triggers, ExplainedTrigger{
			Kind:  "chain",
			Chain: r.ChainName,
			Step:  r.StepIndex,
			Match: match.String(),
			File:  r.FilePath,
		})
	}
	return x, nil
}

// retryText renders the retry policy as a sentence rather than four fields.
//
// The backoff numbers are omitted when there is no retry, because a default
// initial_delay on a job that will never wait is noise dressed as
// configuration (P3: the tool renders truth, and the truth is "it does not
// retry").
func retryText(r jobdef.RetrySpec) string {
	if r.MaxAttempts <= 1 {
		return "none (1 attempt)"
	}
	if r.Backoff == jobdef.BackoffFixed {
		return fmt.Sprintf("%d attempts, %s apart", r.MaxAttempts, r.InitialDelay)
	}
	return fmt.Sprintf("%d attempts, exponential from %s up to %s",
		r.MaxAttempts, r.InitialDelay, r.MaxDelay)
}

// scheduleText renders a schedule the way its file wrote it.
func scheduleText(s jobdef.Schedule) string {
	switch {
	case s.Cron != "" && s.Timezone != "":
		return fmt.Sprintf("cron %s (%s)", s.Cron, s.Timezone)
	case s.Cron != "":
		return "cron " + s.Cron
	default:
		return "every " + s.Every.String()
	}
}

// Jobs the store already sorts; this is for the small in-memory lists above.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
