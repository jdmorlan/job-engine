package cli

import (
	"fmt"
	"os"
	"strings"
)

// Whether a tree of definitions can be got back, which is the question that
// decides whether anything is allowed to delete it (D22/D26).
//
// The engine's position is that job definitions belong in a repository. Not for
// tidiness: a source is a whole tree that travels to a worker, secrets are
// encrypted into it and granted to named machines, and D23 reviews a change to
// either as a diff. None of that exists for a directory of YAML somebody has
// only on their laptop.
//
// `je reset` used to keep the jobs directory unconditionally, which quietly
// asserted the opposite -- that definitions are precious *here*, so this is the
// one place the engine must never touch. If they are in a repository they are
// not precious here at all, and refusing to remove a checkout is the tool
// treating a recoverable directory as irreplaceable. So it asks instead, and
// the answer doubles as the nudge: a directory that cannot be recovered is one
// that is not in a repository yet.

// recoverability describes whether a definitions directory could be got back
// after being deleted, and what to say if not.
type recoverability struct {
	recoverable bool
	why         string // why not, phrased for somebody about to lose it
	fix         string // what to do about it, when there is something
}

// definitionsRecoverable reports whether a tree could be restored from
// somewhere other than this disk.
//
// Recoverable means committed AND pushed. A clean checkout with no remote is a
// single copy that happens to have a .git directory, and treating it as safe
// would be the most expensive kind of wrong.
func definitionsRecoverable(dir string) recoverability {
	if entries, err := os.ReadDir(dir); err != nil || len(entries) == 0 {
		// Nothing there, or nothing readable. Either way there is nothing to
		// lose, and reporting a problem would be inventing one.
		return recoverability{recoverable: true}
	}

	root, err := gitRoot(dir)
	if err != nil {
		return recoverability{
			why: "it is not in a git repository, so this disk is the only copy",
			fix: "je init " + dir + "   sets up the tree and versions it",
		}
	}

	if out, err := runGit(root, "status", "--porcelain"); err != nil {
		return recoverability{why: "git could not say whether it is clean"}
	} else if strings.TrimSpace(out) != "" {
		return recoverability{
			why: "it has uncommitted changes",
			fix: "commit them, or keep them with the default",
		}
	}

	if out, err := runGit(root, "remote"); err != nil || strings.TrimSpace(out) == "" {
		return recoverability{
			why: "it is a git repository with no remote, so this disk is still the only copy",
			fix: "add a remote and push, and it becomes recoverable",
		}
	}

	// Committed is not enough: a commit that exists only here dies with the
	// directory. `@{upstream}` is the branch's own tracking ref, so this asks
	// the precise question -- is what is on disk also somewhere else?
	if _, err := runGit(root, "rev-parse", "--abbrev-ref", "@{upstream}"); err != nil {
		return recoverability{
			why: "this branch is not tracking a remote branch, so its commits are only here",
			fix: "push it, and it becomes recoverable",
		}
	}
	if _, err := runGit(root, "merge-base", "--is-ancestor", "HEAD", "@{upstream}"); err != nil {
		return recoverability{
			why: "it has commits that have not been pushed",
			fix: "push them, and it becomes recoverable",
		}
	}
	return recoverability{recoverable: true}
}

// describe renders the reason a directory is being kept.
func (r recoverability) describe(dir string) string {
	out := fmt.Sprintf("Kept: %s -- %s.", dir, r.why)
	if r.fix != "" {
		out += "\n      " + r.fix
	}
	return out
}
