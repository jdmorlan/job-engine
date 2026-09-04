package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jdmorlan/job-engine/internal/service"
)

func init() {
	register(&Command{
		Name:  "reset",
		Usage: "tear down everything on this machine and start from scratch",
		Local: true,
		Long: "For development, when the fastest way forward is a clean slate.\n\n" +
			"It stops and removes every component here -- control plane, worker and\n" +
			"web client, container or native service -- and deletes the state they\n" +
			"accumulated: the databases, the certificate authority, this machine's\n" +
			"identity and keys, and the docker volumes holding them.\n\n" +
			"Nothing here is a definition. Job definitions live in a repository of\n" +
			"yours and reach the engine as a registered source (D22), so the worst\n" +
			"this can cost you is a re-fetch and the history you chose to discard.\n\n" +
			"It also does not touch a control plane somewhere else, and cannot: a\n" +
			"reset is a local operation by nature. Against a cluster you would be\n" +
			"deleting a namespace, which is not this tool's job.\n\n" +
			"You will be asked to type the data directory's name to confirm, because\n" +
			"there is no undo and the run history is not recoverable.",
		Run: runReset,
	})
}

func runReset(ctx context.Context, env *Env, args []string) error {
	cmd := commands["reset"]
	fs := newFlagSet(cmd, env)
	yes := fs.Bool("yes", false, "do it without asking")
	dryRun := fs.Bool("print", false, "list what would be removed, and remove nothing")
	if extra, err := parseArgs(fs, args); err != nil {
		return err
	} else if len(extra) > 0 {
		return usagef("unexpected argument %q", extra[0])
	}

	plan, skipped := planReset(ctx, env)
	if len(plan) == 0 {
		fmt.Fprintf(env.Stdout, "nothing to remove: %s is already clean\n", env.Layout.Data)
		reportSkipped(env, skipped)
		return nil
	}

	fmt.Fprintf(env.Stdout, "This will remove, on this machine only:\n\n")
	for _, step := range plan {
		fmt.Fprintf(env.Stdout, "  %s\n", step.describe)
	}
	reportSkipped(env, skipped)
	if *dryRun {
		return nil
	}

	// Typed confirmation rather than y/N. The run history is gone afterwards
	// and there is no undo, so the gesture should be proportional -- and typing
	// the directory name is the one confirmation that cannot be given by
	// somebody who has misread which machine they are on.
	if !*yes {
		want := filepath.Base(env.Layout.Data)
		fmt.Fprintf(env.Stderr, "\nThere is no undo. Type %q to confirm: ", want)
		if readLine(env) != want {
			fmt.Fprintln(env.Stdout, "nothing was removed")
			return nil
		}
	}

	var failed int
	for _, step := range plan {
		if err := step.do(ctx, env); err != nil {
			// Keep going. A half-reset that stopped at the first problem is
			// worse than one that did what it could and said what it could not:
			// the next thing somebody does is run it again.
			fmt.Fprintf(env.Stderr, "could not %s: %v\n", step.describe, err)
			failed++
			continue
		}
		fmt.Fprintf(env.Stdout, "removed %s\n", step.describe)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d steps failed; run `je reset` again once they are dealt with",
			failed, len(plan))
	}

	fmt.Fprintf(env.Stdout, "\nclean. Start again with:  je quickstart\n")
	return nil
}

// resetStep is one thing to remove, and how.
type resetStep struct {
	describe string
	do       func(ctx context.Context, env *Env) error
}

// planReset works out what is actually here, so the confirmation lists real
// things rather than a fixed script of what might exist.
//
// Everything machine-wide is checked for ownership first. A container is
// global to the machine and a data directory is not, so a reset run in a
// scratch directory would otherwise remove the containers and volumes of a real
// deployment -- which is exactly what it did the first time this was run.
func planReset(ctx context.Context, env *Env) (plan []resetStep, skipped []string) {

	// Containers first: they hold the ports, and a native service coming back
	// up while a container still owns :7620 fails confusingly.
	volumes := map[string]bool{}
	for _, component := range []string{"control-plane", "worker", "web"} {
		name := containerName(component)
		if component == "control-plane" {
			name = controlPlaneContainer(env.Layout)
		}
		if !containerNamed(ctx, name) {
			continue
		}
		mine, observed := containerBelongsTo(ctx, name, env.Layout.Data)
		if !mine {
			skipped = append(skipped, fmt.Sprintf("container %s -- %s", name, observed))
			continue
		}
		for _, v := range containerVolumes(ctx, name) {
			volumes[v] = true
		}
		plan = append(plan, resetStep{
			describe: "the " + component + " container (" + name + ")",
			do: func(ctx context.Context, env *Env) error {
				out, err := exec.CommandContext(ctx, "docker", "rm", "--force", name).CombinedOutput()
				if err != nil {
					return fmt.Errorf("%s", strings.TrimSpace(lastLine(string(out))))
				}
				return nil
			},
		})
	}

	for _, c := range []service.Component{service.ControlPlane, service.Worker} {
		manager, err := service.New(c)
		if err != nil {
			continue
		}
		state, err := manager.Status()
		if err != nil || !state.Installed {
			continue
		}
		if dir, known := unitDataDir(state.UnitPath); !known || !sameDir(dir, env.Layout.Data) {
			where := "an unknown data directory"
			if known {
				where = dir
			}
			skipped = append(skipped, fmt.Sprintf("the %s service -- runs against %s", c, where))
			continue
		}
		plan = append(plan, resetStep{
			describe: "the " + string(c) + " service (" + state.Manager + ")",
			do:       func(ctx context.Context, env *Env) error { return manager.Uninstall() },
		})
	}

	// Volumes come from the containers being removed rather than from a list
	// of names, so a volume nobody here mounted is never a candidate.
	for _, volume := range sortedKeys(volumes) {
		plan = append(plan, resetStep{
			describe: "docker volume " + volume,
			do: func(ctx context.Context, env *Env) error {
				out, err := exec.CommandContext(ctx, "docker", "volume", "rm", volume).CombinedOutput()
				if err != nil {
					return fmt.Errorf("%s", strings.TrimSpace(lastLine(string(out))))
				}
				return nil
			},
		})
	}

	// Everything in the data directory except the definitions, which are the
	// one thing here a person wrote.
	for _, path := range resettablePaths(env) {
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		plan = append(plan, resetStep{
			describe: relativeTo(filepath.Dir(env.Layout.Data), path),
			do:       func(ctx context.Context, env *Env) error { return os.RemoveAll(path) },
		})
	}
	return plan, skipped
}

// sameDir compares two paths as the same directory, following symlinks where it
// can. A reset must not decline to clean up its own deployment because one path
// went through /private and the other did not.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	resolve := func(p string) string {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

// unitDataDir reads the data directory a service unit was installed against.
//
// The unit records `--data-dir <path>` in the command it runs, so the file
// itself says whose it is. Read as text rather than parsed as plist or ini,
// because the one fact wanted is present in both formats the same way and a
// parser for each would be two more things to keep right.
func unitDataDir(unitPath string) (string, bool) {
	body, err := os.ReadFile(unitPath)
	if err != nil {
		return "", false
	}
	fields := strings.FieldsFunc(string(body), func(r rune) bool {
		return r == '\n' || r == '\r' || r == '\t' || r == ' ' || r == '<' || r == '>' || r == '"'
	})
	for i, f := range fields {
		if f == "--data-dir" && i+1 < len(fields) {
			// The plist wraps each argument in <string></string>, so the next
			// field after splitting on the delimiters above is the value.
			for _, candidate := range fields[i+1:] {
				if candidate != "string" && candidate != "/string" {
					return candidate, true
				}
			}
		}
	}
	return "", false
}

// resettablePaths is the state the engine accumulates, named explicitly.
//
// A list rather than "delete the data directory and keep jobs/", because the
// data directory is somewhere a person may keep things this tool did not put
// there. Removing only what was written here means a reset cannot eat something
// it does not understand.
func resettablePaths(env *Env) []string {
	l := env.Layout
	return []string{
		l.StateDB(), l.StateDB() + "-wal", l.StateDB() + "-shm",
		l.LogsDB(), l.LogsDB() + "-wal", l.LogsDB() + "-shm",
		l.Runtime(), l.Endpoint(), l.Lock(),
		l.CADir(), l.BootstrapDir(),
		l.IdentityCert(), l.IdentityKey(), l.IdentityCA(), l.AgeIdentity(),
		// Fetched source trees. Deleting them costs a re-fetch and nothing
		// else, which is the whole reason they live under a directory called
		// cache (D22).
		l.Cache(),
	}
}

// reportSkipped names what was left alone and why.
//
// The most important output this command has. Something machine-wide that
// belongs to another data directory is exactly what somebody would expect a
// reset to have dealt with, and silence would read as "there was nothing there".
func reportSkipped(env *Env, skipped []string) {
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintf(env.Stderr, "\nLeft alone, because this data directory does not own it:\n")
	for _, s := range skipped {
		fmt.Fprintf(env.Stderr, "  %s\n", s)
	}
	fmt.Fprintf(env.Stderr, "  Reset that deployment from its own data directory:  je --data-dir <dir> reset\n")
}

func readLine(env *Env) string {
	var line string
	fmt.Fscanln(env.Stdin, &line)
	return strings.TrimSpace(line)
}
