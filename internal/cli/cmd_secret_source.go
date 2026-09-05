package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/jdmorlan/job-engine/internal/secretfile"
	"github.com/jdmorlan/job-engine/internal/secrets"

	"golang.org/x/term"
)

// The repository half of `je secret` (D25).
//
// `je secret set NAME` puts a value in the control plane's own store, where the
// control plane can read it. These write into a source's `secrets.enc.yaml`
// instead, where it cannot: names stay cleartext so a declared-but-missing
// secret is still a load-time error, and values are readable only by the
// recipients the file names.
//
// They edit a checkout on this machine and stop. That is deliberate and it is
// D23's argument: "this worker may now read production credentials" should be a
// diff a human approves, not a configuration change nobody sees. Sending the
// edit to the control plane instead would write into the tree it fetched, which
// is a cache the next `je source sync` overwrites -- the change would appear to
// work and then vanish.

// sourceTarget is the checkout a repository-secrets command operates on.
type sourceTarget struct {
	source string
	dir    string // the tree root
	file   string // dir/secrets.enc.yaml
}

// resolveSourceTree finds the working copy to edit.
//
// Explicit --path wins; otherwise it is the git root of the directory you are
// standing in, which has to look like a checkout before anything is written to
// it.
//
// It used to ask the control plane where the source lived and use that when it
// answered. That answer was the *cache* -- a tree keyed by commit that the next
// fetch replaces -- so `je secret set --source` would report success, print the
// path, tell you to commit it, and leave nothing in your repository at all. The
// field it read was documented as "where a directory source reads from", a kind
// D27 deleted, and once every source was a repository it returned the cache for
// all of them.
//
// Which is the failure this file's own comment above says it exists to prevent.
// A comment is not a mechanism: the field is gone now, so the wrong answer is
// not available to be read.
func resolveSourceTree(ctx context.Context, env *Env, c *Client, source, path string) (sourceTarget, error) {
	if path != "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return sourceTarget{}, err
		}
		return sourceTarget{source: source, dir: abs,
			file: filepath.Join(abs, secretfile.Name)}, nil
	}

	listCtx, cancel := withTimeout(ctx)
	defer cancel()
	sources, err := c.Sources(listCtx)
	if err != nil {
		return sourceTarget{}, err
	}
	var known bool
	for _, s := range sources {
		if s.Name == source {
			known = true
			break
		}
	}
	if !known {
		return sourceTarget{}, fmt.Errorf(
			"no source named %q; `je source list` shows the ones registered", source)
	}

	// A repository source. The control plane's copy is a cache, so the only
	// correct place to edit is the checkout the person is standing in.
	cwd, err := os.Getwd()
	if err != nil {
		return sourceTarget{}, err
	}
	root, err := gitRoot(cwd)
	if err != nil {
		return sourceTarget{}, fmt.Errorf(
			"%q is a repository source, so this edits your checkout of it -- and %s\n"+
				"is not inside a git repository.\n\n"+
				"Run this from your clone, or name it:  --path <dir>",
			source, cwd)
	}
	return sourceTarget{source: source, dir: root,
		file: filepath.Join(root, secretfile.Name)}, nil
}

// secretSetInSource encrypts one value into a source's secrets file.
func secretSetInSource(ctx context.Context, env *Env, c *Client, t sourceTarget, name string, commit commitMode) error {
	if !secrets.ValidName(name) {
		return fmt.Errorf("%q is not a valid secret name; use A-Z, digits and underscores", name)
	}
	id, err := ensureAgeIdentity(env)
	if err != nil {
		return err
	}

	file, created, err := openOrCreateSecretFile(t, id)
	if err != nil {
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
	if err := file.Set(id, name, value); err != nil {
		return err
	}
	if err := writeSecretFile(t, file); err != nil {
		return err
	}

	if created {
		fmt.Fprintf(env.Stdout, "created %s, readable by this machine\n", t.file)
	}
	fmt.Fprintf(env.Stdout, "set %s in %s\n", name, t.source)
	if len(value) < secrets.MinRedactableLength {
		fmt.Fprintf(env.Stderr,
			"warning: %s is shorter than %d characters and will NOT be redacted from job logs\n",
			name, secrets.MinRedactableLength)
	}
	return offerCommit(env, t, fmt.Sprintf("secrets(%s): set %s", t.source, name), commit)
}

// secretRecipientsAdd grants an identity the ability to read a source's
// secrets, resolving a name to the key that identity registered (D25).
func secretRecipientsAdd(ctx context.Context, env *Env, c *Client, t sourceTarget, who string, commit commitMode) error {
	id, err := readAgeIdentity(env)
	if err != nil {
		return fmt.Errorf(
			"this machine has no key, so it cannot read %s -- and access is granted\n"+
				"by somebody who has it, never by the control plane on its own.\n"+
				"Make one:  je worker keygen", t.file)
	}

	// A name resolves through the control plane; a key is taken as given. The
	// point of the name is that the control plane learned that key from the
	// machine itself, rather than from somebody pasting it -- so pasting is
	// still allowed and still says what it is.
	recipient, resolved := who, ""
	if !strings.HasPrefix(who, "age1") {
		lookupCtx, cancel := withTimeout(ctx)
		defer cancel()
		key, err := c.AgeKeyFor(lookupCtx, who)
		if err != nil {
			return err
		}
		recipient, resolved = key, who
	}
	if _, err := age.ParseX25519Recipient(recipient); err != nil {
		return fmt.Errorf("%q is neither an enrolled identity nor an age public key", who)
	}

	file, created, err := openOrCreateSecretFile(t, id)
	if err != nil {
		return err
	}
	if err := file.AddRecipient(id, recipient); err != nil {
		return err
	}
	if err := writeSecretFile(t, file); err != nil {
		return err
	}

	if created {
		fmt.Fprintf(env.Stdout, "created %s\n", t.file)
	}
	if resolved != "" {
		fmt.Fprintf(env.Stdout, "%s can now read %s's secrets\n  %s\n",
			resolved, t.source, recipient)
	} else {
		fmt.Fprintf(env.Stdout, "added a recipient to %s\n", t.source)
		fmt.Fprintln(env.Stderr,
			"note: this key was given directly, so nothing checked that it belongs to\n"+
				"      the machine you meant. `je secret recipients add <source> <name>`\n"+
				"      resolves a name to the key that identity registered.")
	}

	// Values already in the file were encrypted to a data key that has just
	// been rewrapped, so the new recipient can read all of them -- including
	// ones set before it was added. Worth saying: it is the difference between
	// this and a per-value grant.
	fmt.Fprintf(env.Stdout, "  it can read every value in the file, including ones set earlier\n")

	message := fmt.Sprintf("secrets(%s): add recipient %s", t.source, who)
	return offerCommit(env, t, message, commit)
}

func secretRecipientsList(env *Env, t sourceTarget) error {
	body, err := os.ReadFile(t.file)
	if err != nil {
		return fmt.Errorf("%s has no encrypted secrets file (%s)", t.source, t.file)
	}
	file, err := secretfile.Parse(body)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s\n\n", t.file)
	fmt.Fprintln(env.Stdout, "recipients (who can decrypt):")
	for _, r := range file.Recipients {
		fmt.Fprintf(env.Stdout, "  %s\n", r)
	}
	names := file.Names()
	fmt.Fprintf(env.Stdout, "\nsecrets (%d, names readable by anyone):\n", len(names))
	for _, n := range names {
		fmt.Fprintf(env.Stdout, "  %s\n", n)
	}
	return nil
}

// openOrCreateSecretFile loads the file, or starts one readable by this machine.
//
// A new file gets exactly one recipient: the key that created it. Any other
// default would be a guess about who should be able to read production
// credentials, and there is no safe guess.
func openOrCreateSecretFile(t sourceTarget, id *age.X25519Identity) (*secretfile.File, bool, error) {
	body, err := os.ReadFile(t.file)
	switch {
	case err == nil:
		file, err := secretfile.Parse(body)
		return file, false, err
	case !os.IsNotExist(err):
		return nil, false, err
	}
	file, err := secretfile.New([]string{id.Recipient().String()})
	return file, true, err
}

func writeSecretFile(t sourceTarget, file *secretfile.File) error {
	body, err := file.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.file), 0o755); err != nil {
		return err
	}
	// 0644: every value in it is encrypted, and the file is meant to be
	// committed. A mode implying otherwise would suggest the wrong thing about
	// what protects it.
	return os.WriteFile(t.file, body, 0o644)
}

// commitMode is what to do about git afterwards.
type commitMode int

const (
	commitAsk commitMode = iota
	commitAlways
	commitNever
)

// offerCommit proposes the commit, because the edit is only half of the change.
//
// A secrets file that is written and not committed is a change that exists on
// one laptop, and under D23 the whole value of putting secrets in the repository
// is that granting access is a reviewable diff. Asking rather than doing keeps
// the tool out of somebody's history uninvited, and offering a conventional
// message means the diff arrives described.
func offerCommit(env *Env, t sourceTarget, message string, mode commitMode) error {
	if mode == commitNever {
		fmt.Fprintf(env.Stdout, "\n%s is modified and not committed.\n", relativeTo(t.dir, t.file))
		return nil
	}
	root, err := gitRoot(t.dir)
	if err != nil {
		fmt.Fprintf(env.Stdout,
			"\n%s is modified. It is not in a git repository, so commit it however\n"+
				"this tree is versioned -- the point of the file is that granting access\n"+
				"is a change somebody can review.\n", t.file)
		return nil
	}

	if mode == commitAsk {
		if !confirm(env, fmt.Sprintf("\ncommit this?  %s\n[y/N] ", message)) {
			fmt.Fprintf(env.Stdout, "left uncommitted: %s\n", relativeTo(root, t.file))
			return nil
		}
	}

	if out, err := runGit(root, "add", "--", t.file); err != nil {
		return fmt.Errorf("git add: %w: %s", err, out)
	}
	// Only the secrets file, so an unrelated staged change is not swept into a
	// commit somebody did not ask for.
	out, err := runGit(root, "commit", "--only", "--message", message, "--", t.file)
	if err != nil {
		return fmt.Errorf("git commit: %w: %s", err, out)
	}
	fmt.Fprintf(env.Stdout, "committed: %s\n", message)
	fmt.Fprintln(env.Stdout, "push it when you are ready; the control plane reads the source on sync")
	return nil
}

// interactive reports whether there is a person to ask.
func interactive(env *Env) bool {
	stdin, ok := env.Stdin.(*os.File)
	return ok && term.IsTerminal(int(stdin.Fd()))
}

func confirm(env *Env, prompt string) bool {
	if !interactive(env) {
		// Not a person: do nothing rather than act unattended. A script that
		// wants it says so with a flag.
		return false
	}
	fmt.Fprint(env.Stderr, prompt)
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func gitRoot(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func relativeTo(base, path string) string {
	if rel, err := filepath.Rel(base, path); err == nil {
		return rel
	}
	return path
}

// secretListInSource shows what a repository holds, which is names and readers
// and never values.
//
// The control plane cannot decrypt these and neither can this command without
// the right key -- so what is listed is exactly what the file leaves in
// cleartext, on purpose: a declared secret that is missing is still a load-time
// error, and nothing else about it is knowable from here (D25).
func secretListInSource(ctx context.Context, env *Env, c *Client, source string) error {
	listCtx, cancel := withTimeout(ctx)
	defer cancel()

	sources, err := c.Sources(listCtx)
	if err != nil {
		return err
	}
	for _, s := range sources {
		if s.Name != source {
			continue
		}
		if s.SecretsError != "" {
			return fmt.Errorf("%s: %s", secretfile.Name, s.SecretsError)
		}
		// Everything below describes the revision the control plane last
		// fetched, and saying so is the whole difference between this being
		// useful and being the confusing answer it replaced. The secrets file
		// travels with the definitions (D25), so one just written into a
		// checkout is invisible here until it is pushed and synced -- and "no
		// secrets" a second after setting one reads exactly like a failure.
		at := "the revision the control plane last fetched"
		if s.Revision != "" {
			at = shortSHA(s.Revision) + ", the revision the control plane last fetched"
		}

		if len(s.Secrets) == 0 {
			fmt.Fprintf(env.Stdout, "no secrets in %s at %s.\n", source, at)
			fmt.Fprintf(env.Stdout,
				"\nIf you have just set one, it is in your checkout and not here yet:\n"+
					"  git add %s && git commit -m \"secrets\" && git push\n"+
					"  je source sync %s\n"+
					"\nOr set one:  je secret set --source %s NAME\n",
				secretfile.Name, source, source)
			return nil
		}

		fmt.Fprintf(env.Stdout, "%d secret(s) in %s at %s, readable by %d key(s)\n\n",
			len(s.Secrets), source, at, len(s.Recipients))
		for _, name := range s.Secrets {
			fmt.Fprintf(env.Stdout, "  %s\n", name)
		}
		// The recipients are the access list, and "who can read production
		// credentials" is the question this file exists to make reviewable.
		if len(s.Recipients) > 0 {
			fmt.Fprintln(env.Stdout, "\nreadable by")
			for _, r := range s.Recipients {
				fmt.Fprintf(env.Stdout, "  %s\n", r)
			}
		}
		return nil
	}
	return fmt.Errorf("no source named %q; `je source` shows the ones registered", source)
}
