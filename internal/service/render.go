package service

import (
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"
)

// Rendering the unit files lives here, apart from the code that runs launchctl
// and systemctl, for one practical reason: these are pure functions and can
// therefore be tested on any machine. A plist that only compiles on a Mac is a
// plist that CI never checks, and the whole job of this file is producing
// something a service manager will accept on the first try.

// plist renders the LaunchAgent.
//
// Built with encoding/xml rather than a template so that a path containing an
// ampersand or a quote cannot produce a plist that silently fails to parse --
// and launchd's response to a malformed plist is to do nothing, quietly, which
// is the worst possible failure for the thing whose job is keeping the daemon
// alive.
func plist(cfg Config, _ string) (string, error) {
	args := []string{cfg.Binary}
	if cfg.DataDir != "" {
		args = append(args, "--data-dir", cfg.DataDir)
	}
	args = append(args, "serve")
	if cfg.Addr != "" {
		args = append(args, "--addr", cfg.Addr)
	}

	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" " +
		"\"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")

	writeKey := func(key string) { fmt.Fprintf(&b, "  <key>%s</key>\n", escape(key)) }
	writeString := func(key, value string) {
		writeKey(key)
		fmt.Fprintf(&b, "  <string>%s</string>\n", escape(value))
	}

	writeString("Label", Label)

	writeKey("ProgramArguments")
	b.WriteString("  <array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", escape(a))
	}
	b.WriteString("  </array>\n")

	// RunAtLoad starts it now and at login; KeepAlive brings it back if it
	// crashes. Together they are the whole point of registering at all (D16).
	writeKey("RunAtLoad")
	b.WriteString("  <true/>\n")
	writeKey("KeepAlive")
	b.WriteString("  <true/>\n")

	writeString("StandardOutPath", cfg.LogPath)
	writeString("StandardErrorPath", cfg.LogPath)
	writeString("WorkingDirectory", filepath.Dir(cfg.DataDir))

	if cfg.Path != "" {
		writeKey("EnvironmentVariables")
		b.WriteString("  <dict>\n")
		fmt.Fprintf(&b, "    <key>PATH</key>\n    <string>%s</string>\n", escape(cfg.Path))
		b.WriteString("  </dict>\n")
	}

	// Ten seconds between restarts. Without it, a daemon that fails at startup
	// -- a corrupt database, a port already taken -- is respawned by launchd as
	// fast as it can exit, which turns a small problem into a hot CPU.
	writeKey("ThrottleInterval")
	b.WriteString("  <integer>10</integer>\n")

	b.WriteString("</dict>\n</plist>\n")
	return b.String(), nil
}

func escape(s string) string {
	var out strings.Builder
	if err := xml.EscapeText(&out, []byte(s)); err != nil {
		return s
	}
	return out.String()
}

// unitFile renders the systemd unit.
func unitFile(cfg Config) string {
	args := cfg.Binary
	if cfg.DataDir != "" {
		args += " --data-dir " + cfg.DataDir
	}
	args += " serve"
	if cfg.Addr != "" {
		args += " --addr " + cfg.Addr
	}

	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=je job engine\n")
	// Schedules are wall-clock and jobs reach the network; starting before
	// either is settled produces a confusing first minute after boot.
	b.WriteString("After=network-online.target time-sync.target\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	fmt.Fprintf(&b, "ExecStart=%s\n", args)
	b.WriteString("Restart=always\n")
	// Ten seconds, matching launchd's throttle. Without it a daemon that fails
	// at startup is respawned as fast as it can exit.
	b.WriteString("RestartSec=10\n")
	if cfg.Path != "" {
		// The same PATH problem launchd has: a unit starts with a minimal
		// environment, and D6 passes PATH through to jobs, so without this
		// every job calling npx or python3 fails only under the daemon.
		fmt.Fprintf(&b, "Environment=PATH=%s\n", cfg.Path)
	}
	fmt.Fprintf(&b, "StandardOutput=append:%s\n", cfg.LogPath)
	fmt.Fprintf(&b, "StandardError=append:%s\n", cfg.LogPath)
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}
