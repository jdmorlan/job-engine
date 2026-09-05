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
}

// At is the layout for one named data directory.
//
// Absolute, always, which is the whole point of it existing next to Resolve.
// `--data-dir ./scratch` used to be stored verbatim, and a relative path is a
// fact about the shell that typed it rather than about the deployment: the
// moment anything runs in a different directory -- a subprocess, a service
// started from /, a system job in its scratch dir (P2) -- it resolves to
// somewhere else, or to nothing.
func At(dir string) (Layout, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Layout{}, fmt.Errorf("resolving data dir %q: %w", dir, err)
	}
	return Layout{Data: abs}, nil
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
	// No jobs directory. Definitions are not read from anywhere the engine
	// owns: they live in a repository and arrive as a registered source, so
	// there is nothing here to configure and JE_JOBS_DIR is gone with it.
	return At(data)
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

// Endpoint records where the control plane for this data directory is, when
// the control plane cannot say so itself.
//
// The runtime file above is written by a daemon into its own data directory,
// which stops working the moment the control plane is a container: it writes
// /var/lib/je/daemon.json inside its volume, and a CLI on the host sees
// nothing. The container is running and every command reports that nothing is.
//
// So the thing that set it up writes down where it put it. That is the same
// principle as generating the unit file rather than documenting it, and it is
// the file `--context` will grow out of when a control plane can be on another
// machine entirely (D19/R2).
func (l Layout) Endpoint() string { return filepath.Join(l.Data, "endpoint.json") }

// SourceTree is where a fetched source is unpacked, keyed by the commit it
// came from.
//
// Content-addressed on purpose (D22): a commit is immutable, so a tree that has
// been fetched once never needs fetching again, and two syncs that resolve to
// the same commit do no work. It also means the path a job ran from is
// reconstructible from the revision recorded against the run.
func (l Layout) SourceTree(source, revision string) string {
	return filepath.Join(l.Cache(), "sources", source, revision)
}

// Cache is everything under the data directory that can be thrown away and
// re-obtained.
//
// Named as a cache because that is what it is: fetched source trees, keyed by
// the commit they came from. Definitions are not kept here and are not kept
// anywhere else the engine owns -- they live in a repository and arrive by
// being fetched, so the copy on this disk is a consequence rather than a
// source of truth (D22). `je reset` deletes this without ceremony.
func (l Layout) Cache() string { return filepath.Join(l.Data, "cache") }

// CA is where the control plane keeps the certificate authority it issues
// worker identities from (D25).
//
// Inside the data directory, at 0700 like the secret store, because the key
// here is what makes every issued identity trustworthy. It is deliberately not
// a separate configurable path: a CA somebody can point somewhere else is a CA
// somebody can point at a directory that is backed up to the wrong place.
func (l Layout) CADir() string { return filepath.Join(l.Data, "ca") }

// CAKey and CACert are the authority's private key and its own certificate.
func (l Layout) CAKey() string  { return filepath.Join(l.CADir(), "ca.key") }
func (l Layout) CACert() string { return filepath.Join(l.CADir(), "ca.crt") }

// BootstrapDir holds what a worker needs to enroll itself: the token, and the
// authority to verify the control plane with (D25).
//
// Its own directory rather than loose in the data directory, because it is the
// one part of a control plane's state that another process is *meant* to read.
// The rest -- the databases, and the secret store -- is not, so a deployment
// that shares this one can do so without handing over everything else. In
// Compose that is a volume mounted into the worker; on one machine it is simply
// a subdirectory the worker can already see.
func (l Layout) BootstrapDir() string { return filepath.Join(l.Data, "bootstrap") }

// BootstrapToken is the enrollment token a running control plane leaves for
// workers that can read BootstrapDir. Written on start, removed on clean
// shutdown, like the runtime file.
func (l Layout) BootstrapToken() string { return filepath.Join(l.BootstrapDir(), "token") }

// BootstrapCA is the authority's certificate, published beside the token so a
// worker can verify the control plane before it enrolls. Public by nature: it is
// what a client checks against, not a secret.
func (l Layout) BootstrapCA() string { return filepath.Join(l.BootstrapDir(), "ca.crt") }

// Identity is this machine's own key material, whichever role it plays: the
// age key that reads encrypted secrets, and the certificate that proves which
// worker it is.
func (l Layout) IdentityCert() string { return filepath.Join(l.Data, "worker.crt") }
func (l Layout) IdentityKey() string  { return filepath.Join(l.Data, "worker.key") }

// AgeIdentity is the key this machine reads encrypted secrets with (D25).
//
// Here rather than computed by whoever needs it, because it was computed twice
// and the two disagreed: `je worker keygen` wrote <data>/identity while a
// worker started by `je up --foreground` looked in <data>/cache/identity, so the key
// you had just been told to create was never found. One definition is the fix.
func (l Layout) AgeIdentity() string { return filepath.Join(l.Data, "identity") }

// Toolchains is where `je worker runtime install` puts language toolchains
// (D28), and Toolchainbin is the directory of theirs that goes on PATH.
//
// Inside the data directory because they are this worker's, disposable, and
// re-obtainable -- and because putting them anywhere shared would mean this
// tool deciding what belongs in /usr/local on somebody's machine.
func (l Layout) Toolchains() string   { return filepath.Join(l.Data, "toolchains") }
func (l Layout) ToolchainBin() string { return filepath.Join(l.Toolchains(), "bin") }

// IdentityCA is the authority a worker keeps after enrolling, so it can go on
// verifying the control plane across restarts without the bootstrap directory
// being mounted (D25).
//
// Written by enrollment beside the identity rather than into BootstrapDir,
// because that directory belongs to the control plane and a worker on another
// machine has never seen it.
func (l Layout) IdentityCA() string { return filepath.Join(l.Data, "ca.crt") }

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
