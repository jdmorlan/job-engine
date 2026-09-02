package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/secrets"

	"golang.org/x/term"
)

func init() {
	register(&Command{
		Name:  "secret",
		Args:  "set|list|rm [name]",
		Usage: "manage the values jobs declare and the engine injects",
		Long: "There is no `get`. The CLI never prints a secret value, and only the\n" +
			"secrets a job declares are injected into it.\n\n" +
			"A job declaring a secret that is not set is a definition error: it shows\n" +
			"as misconfigured in `je jobs` and will not run, rather than failing with\n" +
			"a cryptic exit code hours later.",
		Run: runSecret,
	})
}

func runSecret(ctx context.Context, env *Env, args []string) error {
	cmd := commands["secret"]
	fs := newFlagSet(cmd, env)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return usagef("usage: je secret set|list|rm [name]")
	}

	switch positional[0] {
	case "set":
		if len(positional) != 2 {
			return usagef("usage: je secret set <NAME>")
		}
		return secretSet(env, positional[1])
	case "list":
		if len(positional) != 1 {
			return usagef("usage: je secret list")
		}
		return secretList(ctx, env)
	case "rm":
		if len(positional) != 2 {
			return usagef("usage: je secret rm <NAME>")
		}
		return secretRemove(env, positional[1])
	case "get":
		// Worth an explicit refusal rather than "unknown subcommand". Someone
		// typing this has a mental model to correct, not a typo to fix.
		return fmt.Errorf("there is no `je secret get`; the CLI never prints a secret value.\n" +
			"If you need it, you know where the file is")
	default:
		return usagef("unknown subcommand %q; expected set, list or rm", positional[0])
	}
}

// secretSet reads a value without echoing it and stores it.
//
// It does not need the engine, and therefore does not need the data directory
// lock -- so setting a secret works while the daemon is running, which is the
// only time you actually want to.
func secretSet(env *Env, name string) error {
	if !secrets.ValidName(name) {
		return fmt.Errorf("%q is not a valid secret name; use A-Z, digits and underscores", name)
	}
	if err := env.Layout.EnsureData(); err != nil {
		return err
	}

	value, err := readSecretValue(env, name)
	if err != nil {
		return err
	}
	value = strings.TrimRight(value, "\r\n")
	if value == "" {
		return fmt.Errorf("refusing to store an empty value for %s", name)
	}

	store := secrets.Open(env.Layout.Data)
	if err := store.Set(name, value); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "set %s\n", name)

	if private, err := store.DirectoryIsPrivate(); err == nil && !private {
		// Not fatal, and not something to fix behind their back -- the
		// directory may have been created deliberately. But a data directory
		// the rest of the machine can read is worth saying out loud once.
		fmt.Fprintf(env.Stderr,
			"warning: %s is readable by other users on this machine\n"+
				"         the secret file itself is 0600, but run logs are not\n"+
				"         fix with: chmod 700 %s\n",
			env.Layout.Data, env.Layout.Data)
	}
	if len(value) < secrets.MinRedactableLength {
		// Said now, at the moment it can still be changed, rather than left to
		// be discovered in a log file later.
		fmt.Fprintf(env.Stderr,
			"warning: %s is shorter than %d characters and will NOT be redacted from job logs\n",
			name, secrets.MinRedactableLength)
	}
	return nil
}

// readSecretValue prompts without echo on a terminal, and reads a line from
// stdin otherwise so that `je secret set X < file` works in a script.
func readSecretValue(env *Env, name string) (string, error) {
	stdin, ok := env.Stdin.(*os.File)
	if ok && term.IsTerminal(int(stdin.Fd())) {
		// Prompt on stderr, so stdout stays clean for anything being piped.
		fmt.Fprintf(env.Stderr, "Value for %s (not echoed): ", name)
		value, err := term.ReadPassword(int(stdin.Fd()))
		fmt.Fprintln(env.Stderr)
		if err != nil {
			return "", fmt.Errorf("reading value: %w", err)
		}
		return string(value), nil
	}

	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading value from stdin: %w", err)
	}
	return line, nil
}

func secretRemove(env *Env, name string) error {
	store := secrets.Open(env.Layout.Data)
	if err := store.Delete(name); err != nil {
		return err
	}
	// Deleting a secret can break jobs, and saying so beats letting `je jobs`
	// be the way you find out.
	fmt.Fprintf(env.Stdout, "removed %s\n", name)
	fmt.Fprintln(env.Stderr, "any job declaring it is now misconfigured; check `je jobs`")
	return nil
}

// secretList shows names, when they were set, and which jobs use them (D10).
//
// The third column is the useful one: it answers "what breaks if I rotate
// this?" and "why is this token here?", neither of which a bare list can.
func secretList(ctx context.Context, env *Env) error {
	store := secrets.Open(env.Layout.Data)
	entries, err := store.List()
	if err != nil {
		return err
	}

	// Which jobs declare which secret. Best-effort: if the engine cannot be
	// opened because a daemon holds it, still list the secrets rather than
	// failing outright.
	users := map[string][]string{}
	declaredButUnset := map[string][]string{}
	_ = withEngine(ctx, env, func(ctx context.Context, eng *engine.Engine) error {
		jobs, err := eng.Jobs(ctx)
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for _, e := range entries {
			known[e.Name] = true
		}
		for _, j := range jobs {
			def, _, err := eng.Definition(ctx, j.Slug)
			if err != nil {
				continue
			}
			for _, name := range def.Secrets {
				if known[name] {
					users[name] = append(users[name], j.Slug)
				} else {
					declaredButUnset[name] = append(declaredButUnset[name], j.Slug)
				}
			}
		}
		return nil
	})

	if len(entries) == 0 {
		fmt.Fprintln(env.Stdout, "no secrets set")
	} else {
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSET\tUSED BY")
		for _, e := range entries {
			used := "-"
			if names := users[e.Name]; len(names) > 0 {
				used = strings.Join(names, ", ")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name, humanAge(e.SetAt), used)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	// The inverse view, and the one that actually unblocks you: secrets a job
	// is waiting for.
	for name, jobs := range declaredButUnset {
		fmt.Fprintf(env.Stdout, "\n%s is declared by %s but not set\n  je secret set %s\n",
			name, strings.Join(jobs, ", "), name)
	}
	return nil
}

func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
