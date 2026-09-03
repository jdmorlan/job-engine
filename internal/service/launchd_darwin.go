package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// launchd registers a per-user LaunchAgent.
//
// A user agent rather than a system daemon, deliberately: it needs no sudo, it
// runs as you (so it can reach your files, your keychain and your Shortcuts),
// and it matches the installer putting the binary in ~/.local/bin. A system
// LaunchDaemon would run as root before login, which is wrong for a tool whose
// jobs are your jobs.
type launchd struct {
	component Component
	plistPath string
	uid       int
}

func newLaunchd(c Component) (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &launchd{
		component: c,
		plistPath: filepath.Join(home, "Library", "LaunchAgents", Label(c)+".plist"),
		uid:       os.Getuid(),
	}, nil
}

func (l *launchd) Name() string     { return "launchd" }
func (l *launchd) UnitPath() string { return l.plistPath }

// target is launchd's modern address for a per-user service.
func (l *launchd) target() string { return fmt.Sprintf("gui/%d/%s", l.uid, Label(l.component)) }
func (l *launchd) domain() string { return fmt.Sprintf("gui/%d", l.uid) }

func (l *launchd) Install(cfg Config) error {
	body, err := plist(cfg, l.plistPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o700); err != nil {
		return err
	}
	if err := writeUnit(l.plistPath, body); err != nil {
		return err
	}

	// Boot out any previous version first. Bootstrapping over a loaded service
	// fails, and "already loaded" is the normal state when reinstalling after
	// an upgrade -- which is exactly when you most want this to just work.
	_ = exec.Command("launchctl", "bootout", l.target()).Run()

	if out, err := exec.Command("launchctl", "bootstrap", l.domain(), l.plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Survives a `launchctl disable` from a previous life; harmless otherwise.
	_ = exec.Command("launchctl", "enable", l.target()).Run()
	return nil
}

func (l *launchd) Uninstall() error {
	_ = exec.Command("launchctl", "bootout", l.target()).Run()
	if err := os.Remove(l.plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", l.plistPath, err)
	}
	return nil
}

func (l *launchd) Restart() error {
	if out, err := exec.Command("launchctl", "kickstart", "-k", l.target()).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (l *launchd) Status() (State, error) {
	state := State{
		Installed: exists(l.plistPath),
		UnitPath:  l.plistPath,
		Manager:   "launchd",
	}

	out, err := exec.Command("launchctl", "print", l.target()).CombinedOutput()
	if err != nil {
		// Not loaded. launchctl exits non-zero for an unknown service, which
		// is a normal answer rather than a failure.
		return state, nil
	}
	state.Loaded = true

	// `launchctl print` is a human-readable dump; the two lines worth having
	// are the pid and the last exit status. Parsed leniently on purpose --
	// this is a convenience view, and a format change should degrade to
	// showing less rather than failing.
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(fields) != 2 {
			continue
		}
		key, value := strings.TrimSpace(fields[0]), strings.TrimSpace(fields[1])
		switch key {
		case "pid":
			state.PID, _ = strconv.Atoi(value)
		case "last exit code":
			if value != "0" && value != "(never exited)" {
				state.Detail = "last exit code " + value
			}
		}
	}
	return state, nil
}
