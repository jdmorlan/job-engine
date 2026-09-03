// Package service registers a component with the operating system so it starts
// at login and comes back if it dies.
//
// D16's argument: if the engine is a long-running process on your machine,
// "it's running" cannot depend on you remembering to start it -- the whole
// value proposition dies the first time you reboot and don't notice for a week.
//
// D20 made this a package about *components* rather than about "the daemon".
// A control plane and a worker are separately installable, on different
// machines, and a machine may host either or both -- so everything here is
// parameterised by component and nothing assumes there is one service.
//
// Nobody writes a plist or a unit file by hand. That is the point: the CLI
// knows what a working deployment looks like because it is the thing being
// deployed, and a generated unit is reproducible in a way a wiki page is not.
package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// Component is which half of the engine a service runs (D20, F1).
type Component string

const (
	ControlPlane Component = "control-plane"
	Worker       Component = "worker"
)

// Components is every installable component, for commands that iterate.
var Components = []Component{ControlPlane, Worker}

// labelPrefix is reverse-DNS because launchd expects it, and reused as the
// systemd unit name so there is one string to recognise in logs on either
// platform.
const labelPrefix = "io.github.jdmorlan.je"

// Label is the OS-level identifier for one component's service.
//
// Per-component rather than one label for "je", because a machine running both
// halves has two services with independent lifecycles: restarting a worker must
// not touch the control plane holding the database.
func Label(c Component) string { return labelPrefix + "." + string(c) }

// Config describes the service to register.
type Config struct {
	// Component decides the label, the unit path, the log file, and which
	// subcommand the unit runs.
	Component Component

	// Binary is the absolute path to `je`. Absolute because a service manager
	// runs with a minimal environment and will not search a PATH you assume.
	Binary string

	DataDir string

	// Args are the component's own flags, after its subcommand: `--addr` for a
	// control plane, `--addr --name --labels` for a worker. Held as a list so
	// the renderer stays ignorant of what either component takes, and adding a
	// flag is a change in one place.
	Args []string

	// LogPath receives the daemon's own stdout and stderr. Job output goes to
	// the logs database (D4); this is the engine talking about itself.
	LogPath string

	// Path is the PATH the daemon runs with, and it matters more than it
	// looks. A service manager starts processes with a minimal PATH --
	// launchd gives you /usr/bin:/bin:/usr/sbin:/sbin -- and D6 passes PATH
	// through to jobs. Without capturing the installing shell's PATH, every
	// job that runs `npx`, `python3` or anything from Homebrew fails with
	// "command not found" only when run by the daemon, and works when you run
	// it by hand. That is a genuinely horrible afternoon.
	Path string
}

// State is what the OS thinks of the service.
type State struct {
	// Installed reports that the unit file exists.
	Installed bool
	// Loaded reports that the service manager knows about it.
	Loaded bool
	// PID is the running process, or 0.
	PID int
	// UnitPath is where the definition lives, so a human can go read it.
	UnitPath string
	// Manager names the system doing the work: launchd or systemd.
	Manager string
	// Detail carries anything platform-specific worth showing.
	Detail string
}

// Manager registers and inspects the service.
type Manager interface {
	Install(cfg Config) error
	Uninstall() error
	Status() (State, error)
	Restart() error
	UnitPath() string
	Name() string
}

// writeUnit writes a unit file atomically, creating its directory.
func writeUnit(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".je-unit-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
