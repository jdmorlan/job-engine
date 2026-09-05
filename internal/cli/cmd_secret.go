package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jdmorlan/job-engine/internal/secrets"

	"golang.org/x/term"
)

func init() {
	register(&Command{
		Name:  "secret",
		Args:  "set|list|rm|recipients [name]",
		Usage: "manage the values jobs declare and the engine injects",
		Long: "There is no `get`. The CLI never prints a secret value, and only the\n" +
			"secrets a job declares are injected into it.\n\n" +
			"A job declaring a secret that is not set is a definition error: it shows\n" +
			"as misconfigured in `je jobs` and will not run, rather than failing with\n" +
			"a cryptic exit code hours later.\n\n" +
			"Two places a secret can live, and the difference is who can read it:\n\n" +
			"  je secret set NAME              the control plane's own store. It holds\n" +
			"                                  the value and can redact it from logs.\n" +
			"  je secret set --source X NAME   encrypted into source X's repository,\n" +
			"                                  readable only by the recipients the file\n" +
			"                                  names. The control plane cannot read it,\n" +
			"                                  and the worker decrypts it (D25).\n\n" +
			"The second is what lets a worker on a machine you do not fully control run\n" +
			"a job that needs a credential. It edits your checkout and offers to commit,\n" +
			"because granting access should be a diff somebody reviews.\n\n" +
			"subcommands:\n" +
			"  set         store a value, in one place or the other\n" +
			"  list        names, when they were set, and which jobs use them\n" +
			"  rm          remove one from the control plane's store\n" +
			"  recipients  who can read a source's encrypted secrets, and grant more",
		Run: runSecret,
	})
}

func runSecret(ctx context.Context, env *Env, args []string) error {
	cmd := commands["secret"]
	fs := newFlagSet(cmd, env)
	source := fs.String("source", "", "encrypt into this source's repository instead of the control plane's store")
	path := fs.String("path", "", "the checkout to edit (default: a directory source's path, or this git repository)")
	doCommit := fs.Bool("commit", false, "commit the change without asking")
	noCommit := fs.Bool("no-commit", false, "leave the change uncommitted, and do not ask")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		return usagef("usage: je secret set|list|rm|recipients [name]")
	}

	mode := commitAsk
	switch {
	case *doCommit && *noCommit:
		return usagef("--commit and --no-commit ask for opposite things")
	case *doCommit:
		mode = commitAlways
	case *noCommit:
		mode = commitNever
	}

	switch positional[0] {
	case "set":
		if len(positional) != 2 {
			return usagef("usage: je secret set [--source <src>] <NAME>")
		}
		if *source != "" {
			return withClient(ctx, env, func(ctx context.Context, c *Client) error {
				target, err := resolveSourceTree(ctx, env, c, *source, *path)
				if err != nil {
					return err
				}
				return secretSetInSource(ctx, env, c, target, positional[1], mode)
			})
		}
		return withClient(ctx, env, func(ctx context.Context, c *Client) error {
			return secretSet(ctx, env, c, positional[1])
		})
	case "recipients":
		if len(positional) < 2 {
			return usagef("usage: je secret recipients list|add --source <src> [name-or-key]")
		}
		if *source == "" {
			return usagef("je secret recipients needs --source <src>: recipients are a " +
				"property of one repository's secrets file")
		}
		return withClient(ctx, env, func(ctx context.Context, c *Client) error {
			target, err := resolveSourceTree(ctx, env, c, *source, *path)
			if err != nil {
				return err
			}
			switch positional[1] {
			case "list":
				return secretRecipientsList(env, target)
			case "add":
				if len(positional) != 3 {
					return usagef("usage: je secret recipients add --source <src> <name-or-key>")
				}
				return secretRecipientsAdd(ctx, env, c, target, positional[2], mode)
			default:
				return usagef("unknown subcommand %q; expected list or add", positional[1])
			}
		})
	case "list":
		if len(positional) != 1 {
			return usagef("usage: je secret list [--source <src>]")
		}
		return withClient(ctx, env, func(ctx context.Context, c *Client) error {
			// --source used to parse and then be dropped, so `je secret list
			// --source x` reported the control plane's store and said "no
			// secrets set" to somebody who had just set one in x. Two stores
			// exist on purpose (D25); a flag naming one of them and being
			// ignored is how that turns from a design into a trap.
			if *source != "" {
				return secretListInSource(ctx, env, c, *source)
			}
			return secretList(ctx, env, c)
		})
	case "rm":
		if len(positional) != 2 {
			return usagef("usage: je secret rm <NAME>")
		}
		return withClient(ctx, env, func(ctx context.Context, c *Client) error {
			return secretRemove(ctx, env, c, positional[1])
		})
	case "get":
		// Worth an explicit refusal rather than "unknown subcommand". Someone
		// typing this has a mental model to correct, not a typo to fix.
		return fmt.Errorf("there is no `je secret get`; the CLI never prints a secret value")
	default:
		return usagef("unknown subcommand %q; expected set, list, rm or recipients",
			positional[0])
	}
}

// secretSet reads a value without echoing it and sends it to the control plane.
//
// The name is validated here as well as server-side, because refusing before
// prompting is kinder than taking a value and then rejecting it.
func secretSet(ctx context.Context, env *Env, c *Client, name string) error {
	if !secrets.ValidName(name) {
		return fmt.Errorf("%q is not a valid secret name; use A-Z, digits and underscores", name)
	}

	value, err := readSecretValue(env, name)
	if err != nil {
		return err
	}
	value = strings.TrimRight(value, "\r\n")
	if value == "" {
		return fmt.Errorf("refusing to store an empty value for %s", name)
	}

	setCtx, cancel := withTimeout(ctx)
	defer cancel()

	result, err := c.SetSecret(setCtx, name, value)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "set %s\n", result.Name)

	// Both warnings are facts about the control plane's own filesystem, so it
	// reports them and this renders them. They are said now, while the value
	// can still be changed, rather than left to be discovered in a log later.
	if !result.DirectoryPrivate {
		fmt.Fprintf(env.Stderr,
			"warning: the control plane's data directory is readable by other users\n"+
				"         the secret file itself is 0600, but run logs are not\n")
	}
	if !result.Redactable {
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

func secretRemove(ctx context.Context, env *Env, c *Client, name string) error {
	rmCtx, cancel := withTimeout(ctx)
	defer cancel()

	if err := c.DeleteSecret(rmCtx, name); err != nil {
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
// this?" and "why is this token here?", neither of which a bare list can. The
// join is done by the control plane, so this only renders.
func secretList(ctx context.Context, env *Env, c *Client) error {
	listCtx, cancel := withTimeout(ctx)
	defer cancel()

	view, err := c.Secrets(listCtx)
	if err != nil {
		return err
	}

	if len(view.Secrets) == 0 {
		fmt.Fprintln(env.Stdout, "no secrets set")
	} else {
		tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSET\tUSED BY")
		for _, e := range view.Secrets {
			used := "-"
			if len(e.Jobs) > 0 {
				used = strings.Join(e.Jobs, ", ")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name, humanAge(e.SetAt), used)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	// The inverse view, and the one that actually unblocks you: secrets a job
	// is waiting for.
	for _, u := range view.Unset {
		fmt.Fprintf(env.Stdout, "\n%s is declared by %s but not set\n  je secret set %s\n",
			u.Name, strings.Join(u.Jobs, ", "), u.Name)
	}

	if !view.DirectoryPrivate {
		fmt.Fprintln(env.Stderr,
			"\nwarning: the control plane's data directory is readable by other users")
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
