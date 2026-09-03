package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// unitName is what systemd calls a component's service. Not the reverse-DNS
// label: systemd units are conventionally short names, and `systemctl --user
// status je-worker` is what somebody will type.
func unitName(c Component) string { return "je-" + string(c) + ".service" }

// systemd registers a per-user unit, for the same reasons launchd gets a user
// agent: no sudo, runs as you, and matches an install into ~/.local/bin.
type systemd struct {
	component Component
	unitPath  string
}

func newSystemd(c Component) (Manager, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, fmt.Errorf("systemctl not found; run `je %s run` yourself, "+
			"or add it to whatever supervises processes here", c)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(home, ".config")
	}
	return &systemd{
		component: c,
		unitPath:  filepath.Join(dir, "systemd", "user", unitName(c)),
	}, nil
}

func (s *systemd) Name() string     { return "systemd" }
func (s *systemd) UnitPath() string { return s.unitPath }

func (s *systemd) Install(cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o700); err != nil {
		return err
	}
	if err := writeUnit(s.unitPath, unitFile(cfg)); err != nil {
		return err
	}

	if err := s.run("daemon-reload"); err != nil {
		return err
	}
	// --now so install means installed and running, rather than installed and
	// waiting for a logout.
	if err := s.run("enable", "--now", unitName(s.component)); err != nil {
		return err
	}

	// Without lingering, a user service stops when the last session ends,
	// which quietly defeats the point on a headless box. Best effort: it needs
	// privileges we may not have, and saying nothing would be worse than
	// trying and letting the status view show the result.
	_ = exec.Command("loginctl", "enable-linger", os.Getenv("USER")).Run()
	return nil
}

func (s *systemd) Uninstall() error {
	_ = s.run("disable", "--now", unitName(s.component))
	if err := os.Remove(s.unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", s.unitPath, err)
	}
	return s.run("daemon-reload")
}

func (s *systemd) Restart() error { return s.run("restart", unitName(s.component)) }

func (s *systemd) Status() (State, error) {
	state := State{
		Installed: exists(s.unitPath),
		UnitPath:  s.unitPath,
		Manager:   "systemd",
	}

	out, err := exec.Command("systemctl", "--user", "show", unitName(s.component),
		"--property=ActiveState,MainPID,Result").CombinedOutput()
	if err != nil {
		return state, nil
	}

	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			state.Loaded = value == "active" || value == "activating"
		case "MainPID":
			state.PID, _ = strconv.Atoi(value)
		case "Result":
			if value != "" && value != "success" {
				state.Detail = "last result: " + value
			}
		}
	}
	return state, nil
}

func (s *systemd) run(args ...string) error {
	full := append([]string{"--user"}, args...)
	if out, err := exec.Command("systemctl", full...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
