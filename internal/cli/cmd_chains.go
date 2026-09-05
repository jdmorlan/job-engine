package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
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
			// Not an error, and not silence either: a chain file in the wrong
			// place is the most likely reason somebody is running this
			// command, so say where they go.
			fmt.Fprintln(env.Stdout, "no chains loaded")
			fmt.Fprintln(env.Stdout, env.Style.Muted(
				"  chain files go in chains/<name>.yaml in a source's repository"))
			env.hint("  ", "je source")
			return nil
		}

		st := env.Style
		tw := env.table()
		fmt.Fprintln(tw, st.Header("CHAIN\tSTEPS\tLAST\tSTATE"))
		attention := false
		for _, c := range chains {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n",
				c.Name, len(c.Steps), st.Muted(chainLastAt(c)), chainStateShort(c, st))
			attention = attention || c.State == engine.ChainStopped
		}
		tw.Flush()

		if attention {
			fmt.Fprintln(env.Stdout)
			env.hint("", "je chain <name>")
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

		st := env.Style
		fmt.Fprintf(env.Stdout, "%s\n", st.Title(c.Name))
		if c.Description != "" {
			fmt.Fprintf(env.Stdout, "%s\n", c.Description)
		}
		fmt.Fprintf(env.Stdout, "%s\n\n", st.Muted(c.FilePath))

		tw := env.table()
		if c.Trigger != nil {
			// The run upstream of step 1. Shown because "what set this off" is
			// half of what somebody opening this view wants to know.
			fmt.Fprintf(tw, "  %s\t%s\t%s\n",
				st.Header("trigger"), c.Trigger.Job, chainRunText(c.Trigger, st))
		}
		for _, s := range c.Steps {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
				st.Header(fmt.Sprintf("step %d", s.Step)), s.Job,
				chainRunText(s.Run, st), st.Muted(fmt.Sprintf("on %s", s.On)))
		}
		tw.Flush()

		fmt.Fprintf(env.Stdout, "\n%s\n", chainStateText(c, st))
		if c.State == engine.ChainStopped {
			return errAttention
		}
		return nil
	})
}

// chainRunText renders the run a step started, or its absence.
func chainRunText(r *engine.ChainRun, st Style) string {
	if r == nil {
		// Deliberately not "failed" or "pending": the step did not fire, and
		// the honest thing to say is that nothing happened here.
		return st.Muted("-") + "\t" + st.Muted("did not fire")
	}
	id := st.Muted(fmt.Sprintf("run %d", r.ID))
	status := st.State(string(r.Status))
	switch {
	case r.EndedAt != nil && r.StartedAt != nil:
		// Millisecond precision, the same as `je runs`: a step that took 31ms
		// and a step that took 900ms are different facts, and rounding both to
		// "0s" hides the one thing a per-step view is for.
		return fmt.Sprintf("%s\t%s in %s", id, status,
			r.EndedAt.Sub(*r.StartedAt).Round(time.Millisecond))
	case r.StartedAt != nil:
		return fmt.Sprintf("%s\t%s for %s", id, status,
			time.Since(*r.StartedAt).Round(time.Millisecond))
	default:
		return fmt.Sprintf("%s\t%s", id, status)
	}
}

// chainStateShort is the table cell: the fact, without the explanation.
func chainStateShort(c engine.ChainView, st Style) string {
	switch c.State {
	case engine.ChainComplete:
		return st.Good("complete") + " " + st.Muted("("+c.Duration.Round(time.Millisecond).String()+")")
	case engine.ChainStopped:
		return st.Bad("stopped at " + failedSteps(c))
	case engine.ChainUnstarted:
		return st.Warn("stalled")
	default:
		return string(c.State)
	}
}

// emptyChainNote is what to say about a file that wires nothing.
const emptyChainNote = "no steps yet -- this file wires nothing"

// chainStateText says what happened in the words the state means.
func chainStateText(c engine.ChainView, st Style) string {
	switch c.State {
	case engine.ChainComplete:
		return st.Good(fmt.Sprintf("complete, %s end to end",
			c.Duration.Round(time.Millisecond)))
	case engine.ChainStopped:
		return st.Bad("stopped at "+failedSteps(c)) + st.Muted(
			" -- nothing downstream of that fired, because the event it waits for never happened")
	case engine.ChainRunning:
		return st.Good("running")
	case engine.ChainUnstarted:
		return st.Warn("stalled") + st.Muted(": a step succeeded and the next one never fired (") +
			st.Cmd("je events") + st.Muted(" shows why)")
	case engine.ChainEmpty:
		return st.Warn(emptyChainNote)
	default:
		return st.Muted("never run")
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
