// Package paths resolves the on-disk locations the engine uses.
//
// It exists as its own package so that exactly one place in the codebase knows
// where things live. Everything else takes a Layout and asks it.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout is the set of paths a single engine instance owns.
//
// D18 makes multiple instances a real possibility (Almanac may start its own
// daemon), so nothing here reads a package-level global -- an instance is
// entirely described by its data directory.
type Layout struct {
	// Data is the engine's data directory. Everything below is inside it,
	// which is what makes "one directory to back up" true.
	Data string

	// Jobs is where job and chain definitions are read from. It defaults to
	// <Data>/jobs but is separately overridable, because D2 expects it to
	// often be a git repo you already have somewhere else.
	Jobs string
}

// Resolve picks the layout from the environment, applying the documented
// precedence: explicit override, then XDG, then a dotdir in $HOME.
func Resolve() (Layout, error) {
	data := os.Getenv("JE_DATA_DIR")
	if data == "" {
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			data = filepath.Join(xdg, "je")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return Layout{}, fmt.Errorf("locating home directory: %w", err)
			}
			data = filepath.Join(home, ".je")
		}
	}
	abs, err := filepath.Abs(data)
	if err != nil {
		return Layout{}, fmt.Errorf("resolving data dir %q: %w", data, err)
	}

	jobs := os.Getenv("JE_JOBS_DIR")
	if jobs == "" {
		jobs = filepath.Join(abs, "jobs")
	} else if jobs, err = filepath.Abs(jobs); err != nil {
		return Layout{}, fmt.Errorf("resolving jobs dir: %w", err)
	}

	return Layout{Data: abs, Jobs: jobs}, nil
}

// StateDB holds runs, events, definitions and cursors (Appendix A).
func (l Layout) StateDB() string { return filepath.Join(l.Data, "state.db") }

// LogsDB holds captured job output, separately from state (D4).
func (l Layout) LogsDB() string { return filepath.Join(l.Data, "logs.db") }

// Lock is the file the daemon flocks to guarantee a single writer.
// Two daemons on one SQLite file would double-fire every schedule (D15, D19).
func (l Layout) Lock() string { return filepath.Join(l.Data, "lock") }

// Runtime records the address and pid of the running daemon so a CLI
// invocation can find it without being told. It is written on start and
// removed on clean shutdown; a stale file is a symptom, not an error.
func (l Layout) Runtime() string { return filepath.Join(l.Data, "daemon.json") }

// Chains is where chain files live -- one flow per file, filename is the
// chain name (D17).
func (l Layout) Chains() string { return filepath.Join(l.Jobs, "chains") }

// EnsureData creates the data directory if it is missing.
//
// 0700 because the secret store (D10) will live in here. Getting the
// permission right at creation is easier than tightening it later.
func (l Layout) EnsureData() error {
	if err := os.MkdirAll(l.Data, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	return nil
}
