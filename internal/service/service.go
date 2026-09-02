// Package service registers the daemon with the operating system so it starts
// at login and comes back if it dies.
//
// D16's argument: if the engine is a long-running process on your machine,
// "it's running" cannot depend on you remembering to start it -- the whole
// value proposition dies the first time you reboot and don't notice for a
// week.
//
// Named `je service install` rather than `je install`, because once there is a
// real installer, "install je" and "install the daemon" are different acts on
// different things and one word for both is a trap.
package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// Label identifies the service to the OS. Reverse-DNS because launchd expects
// it, and reused as the systemd unit name so there is one string to recognise
// in logs on either platform.
const Label = "io.github.jdmorlan.je"

// Config describes the service to register.
type Config struct {
	// Binary is the absolute path to `je`. Absolute because a service manager
	// runs with a minimal environment and will not search a PATH you assume.
	Binary string

	DataDir string
	Addr    string

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
