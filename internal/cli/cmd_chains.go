package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/jobdef"
)

func init() {
	register(&Command{
		Name:  "chains",
		Usage: "list the flows wiring jobs together, and how the last pass went",
		Long: "A chain is one flow per file: chains/<name>.yaml in the jobs\n" +
			"directory, naming the jobs it wires and what triggers each one.\n\n" +
			"Chains are how routes are named and grouped; routes are still what\n" +
			"executes. There is no chain-level lock and no chain state machine --\n" +
			"each step fires as an ordinary trigger, and this view reads the\n" +
			"result back off the causation the engine already records.\n\n" +
			"Exits 3 when a chain's last pass stopped part-way, so it is usable\n" +
			"in a script.",
		Run: runChains,
	})

	register(&Command{
		Name:  "chain",
		Args:  "<name>",
		Usage: "show one chain's most recent pass, step by step",
		Long: "Every step, the run it started, and how long the whole thing took\n" +
			"end to end -- which is the number no job-level view can produce.\n\n" +
			"Exits 3 when the last pass stopped part-way.",
		Run: runChain,
	})
}

func runChains(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["chains"], env)
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		chains, err := rd.Chains(ctx)
		if err != nil {
			return err
		}
		if len(chains) == 0 {
			health, err := rd.Health(ctx)
			if err != nil {
				return err
			}
			// Not an error, and not silence either: a chain file in the wrong
			// place is the most likely reason somebody is running this
			// command, so say where they go.
			fmt.Fprintf(env.Stdout,
				"no chains loaded\n  chain files go in %s/chains/<name>.yaml\n",
				health.JobsDir)
			return nil
		}

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CHAIN\tSTEPS\tLAST\tSTATE")
		attention := false
		for _, c := range chains {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n",
				c.Name, len(c.Steps), chainLastAt(c), chainStateShort(c))
			attention = attention || c.State == engine.ChainStopped
		}
		tw.Flush()

		if attention {
			fmt.Fprintln(env.Stdout, "\nje chain <name> shows where it stopped")
			return errAttention
		}
		return nil
	})
}

func runChain(ctx context.Context, env *Env, args []string) error {
	fs := newFlagSet(commands["chain"], env)
	rest, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usagef("give exactly one chain name")
	}

	return withClient(ctx, env, func(ctx context.Context, rd *Client) error {
		c, err := rd.Chain(ctx, rest[0])
		if err != nil {
			return err
		}

		fmt.Fprintf(env.Stdout, "%s\n", c.Name)
		if c.Description != "" {
			fmt.Fprintf(env.Stdout, "%s\n", c.Description)
		}
		fmt.Fprintf(env.Stdout, "%s\n\n", c.FilePath)

		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		if c.Trigger != nil {
			// The run upstream of step 1. Shown because "what set this off" is
			// half of what somebody opening this view wants to know.
			fmt.Fprintf(tw, "  trigger\t%s\t%s\n", c.Trigger.Job, chainRunText(c.Trigger))
		}
		for _, s := range c.Steps {
			fmt.Fprintf(tw, "  step %d\t%s\t%s\ton %s\n",
				s.Step, s.Job, chainRunText(s.Run), matchText(s.On))
		}
		tw.Flush()

		fmt.Fprintf(env.Stdout, "\n%s\n", chainStateText(c))
		if c.State == engine.ChainStopped {
			return errAttention
		}
		return nil
	})
}

// chainRunText renders the run a step started, or its absence.
func chainRunText(r *engine.ChainRun) string {
	if r == nil {
		// Deliberately not "failed" or "pending": the step did not fire, and
		// the honest thing to say is that nothing happened here.
		return "-\tdid not fire"
	}
	switch {
	case r.EndedAt != nil && r.StartedAt != nil:
		// Millisecond precision, the same as `je runs`: a step that took 31ms
		// and a step that took 900ms are different facts, and rounding both to
		// "0s" hides the one thing a per-step view is for.
		return fmt.Sprintf("run %d\t%s in %s", r.ID, r.Status,
			r.EndedAt.Sub(*r.StartedAt).Round(time.Millisecond))
	case r.StartedAt != nil:
		return fmt.Sprintf("run %d\t%s for %s", r.ID, r.Status,
			time.Since(*r.StartedAt).Round(time.Millisecond))
	default:
		return fmt.Sprintf("run %d\t%s", r.ID, r.Status)
	}
}

// chainStateShort is the table cell: the fact, without the explanation.
func chainStateShort(c engine.ChainView) string {
	switch c.State {
	case engine.ChainComplete:
		return fmt.Sprintf("complete (%s)", c.Duration.Round(time.Millisecond))
	case engine.ChainStopped:
		return "stopped at " + failedSteps(c)
	case engine.ChainUnstarted:
		return "stalled"
	default:
		return string(c.State)
	}
}

// chainStateText says what happened in the words the state means.
func chainStateText(c engine.ChainView) string {
	switch c.State {
	case engine.ChainComplete:
		return fmt.Sprintf("complete, %s end to end", c.Duration.Round(time.Millisecond))
	case engine.ChainStopped:
		return fmt.Sprintf("stopped at %s -- nothing downstream of that fired, "+
			"because the event it waits for never happened", failedSteps(c))
	case engine.ChainRunning:
		return "running"
	case engine.ChainUnstarted:
		return "stalled: a step succeeded and the next one never fired (je events shows why)"
	default:
		return "never run"
	}
}

// failedSteps names every step whose run did not succeed.
//
// Plural because a chain is a set of rules: four reports off one extract fail
// independently, and reporting the first as though it were the whole problem
// would send somebody to fix a quarter of it.
func failedSteps(c engine.ChainView) string {
	parts := make([]string, 0, len(c.Failed))
	for _, n := range c.Failed {
		step := c.Steps[n-1]
		parts = append(parts, fmt.Sprintf("step %d: %s %s", n, step.Job, step.Run.Status))
	}
	return strings.Join(parts, ", ")
}

// chainLastAt is when the chain last did anything, for the list view.
func chainLastAt(c engine.ChainView) string {
	var last *time.Time
	for _, s := range c.Steps {
		if s.Run == nil {
			continue
		}
		if s.Run.EndedAt != nil {
			last = s.Run.EndedAt
		} else if s.Run.StartedAt != nil {
			last = s.Run.StartedAt
		}
	}
	if last == nil {
		return "-"
	}
	return sinceText(last)
}

// matchText renders an event pattern the way the file wrote it.
func matchText(m jobdef.Match) string {
	if len(m.Where) == 0 {
		return m.Event
	}
	keys := make([]string, 0, len(m.Where))
	for k := range m.Where {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%s", k, m.Where[k]))
	}
	return m.Event + " " + strings.Join(pairs, " ")
}
