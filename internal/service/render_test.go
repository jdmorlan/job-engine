package service

import (
	"encoding/xml"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{
		Binary:  "/Users/you/.local/bin/je",
		DataDir: "/Users/you/.je",
		Addr:    "127.0.0.1:7620",
		LogPath: "/Users/you/.je/daemon.log",
		Path:    "/opt/homebrew/bin:/usr/bin:/bin",
	}
}

func TestPlistIsWellFormedXML(t *testing.T) {
	body, err := plist(testConfig(), "")
	if err != nil {
		t.Fatalf("plist: %v", err)
	}
	// launchd's response to a malformed plist is to do nothing, quietly, which
	// is the worst possible failure for the thing whose job is keeping the
	// daemon alive. So this is checked rather than assumed.
	var parsed any
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("plist is not valid XML: %v\n%s", err, body)
	}
}

func TestPlistCarriesTheThingsThatMatter(t *testing.T) {
	body, err := plist(testConfig(), "")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		Label,
		"<key>RunAtLoad</key>", // starts at login (D16)
		"<key>KeepAlive</key>", // comes back after a crash (D16)
		"ThrottleInterval",     // a failing daemon must not spin
		"/Users/you/.local/bin/je",
		"--data-dir",
		"serve",
		"127.0.0.1:7620",
		"/opt/homebrew/bin:/usr/bin:/bin", // the PATH gotcha
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plist is missing %q:\n%s", want, body)
		}
	}
}

// TestPlistEscapesPaths covers the failure that is invisible until boot.
// A home directory containing an ampersand -- or any of XML's five specials --
// would produce a plist that launchd silently refuses to parse.
func TestPlistEscapesPaths(t *testing.T) {
	cfg := testConfig()
	cfg.DataDir = `/Users/a&b/"quoted"/<dir>`
	cfg.Binary = `/Users/a&b/bin/je`

	body, err := plist(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed any
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("a path with XML specials produced invalid XML: %v\n%s", err, body)
	}
	if strings.Contains(body, "a&b") {
		t.Error("the ampersand was not escaped")
	}
	if !strings.Contains(body, "a&amp;b") {
		t.Error("the escaped form is missing")
	}
}

func TestSystemdUnitCarriesTheThingsThatMatter(t *testing.T) {
	body := unitFile(testConfig())

	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"Restart=always", // comes back after a crash (D16)
		"RestartSec=10",  // a failing daemon must not spin
		"WantedBy=default.target",
		"Environment=PATH=/opt/homebrew/bin:/usr/bin:/bin", // the PATH gotcha
		"ExecStart=/Users/you/.local/bin/je --data-dir /Users/you/.je serve --addr 127.0.0.1:7620",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unit is missing %q:\n%s", want, body)
		}
	}
}

// TestUnitsOmitPathWhenUnset keeps the generated file honest: an empty
// Environment line would override the manager's default with nothing.
func TestUnitsOmitPathWhenUnset(t *testing.T) {
	cfg := testConfig()
	cfg.Path = ""

	body, err := plist(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "EnvironmentVariables") {
		t.Error("plist declares an empty environment")
	}
	if strings.Contains(unitFile(cfg), "Environment=PATH=") {
		t.Error("unit sets an empty PATH, which is worse than not setting one")
	}
}
