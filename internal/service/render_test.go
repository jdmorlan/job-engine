package service

import (
	"encoding/xml"
	"strings"
	"testing"
)

func testConfig() Config {
	return Config{
		Component: ControlPlane,
		Binary:    "/Users/you/.local/bin/je",
		DataDir:   "/Users/you/.je",
		Args:      []string{"--addr", "127.0.0.1:7620"},
		LogPath:   "/Users/you/.je/control-plane.log",
		Path:      "/opt/homebrew/bin:/usr/bin:/bin",
	}
}

func workerConfig() Config {
	return Config{
		Component: Worker,
		Binary:    "/Users/you/.local/bin/je",
		DataDir:   "/Users/you/.je",
		Args:      []string{"--addr", "127.0.0.1:7620", "--name", "macbook", "--labels", "macos"},
		LogPath:   "/Users/you/.je/worker.log",
		Path:      "/opt/homebrew/bin:/usr/bin:/bin",
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
		Label(ControlPlane),
		"<key>RunAtLoad</key>", // starts at login (D16)
		"<key>KeepAlive</key>", // comes back after a crash (D16)
		"ThrottleInterval",     // a failing daemon must not spin
		"/Users/you/.local/bin/je",
		"--data-dir",
		"control-plane",
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
		"ExecStart=/Users/you/.local/bin/je --data-dir /Users/you/.je control-plane run --addr 127.0.0.1:7620",
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

// TestComponentsGetSeparateServices is the D20 property this package exists to
// hold: a machine running both halves has two independent services.
//
// One label for "je" would mean restarting a worker also restarts the control
// plane holding the database -- which is exactly the coupling C1 and C2 spent
// their effort removing.
func TestComponentsGetSeparateServices(t *testing.T) {
	if Label(ControlPlane) == Label(Worker) {
		t.Fatal("both components would register as the same service")
	}

	cp, err := plist(testConfig(), "")
	if err != nil {
		t.Fatal(err)
	}
	w, err := plist(workerConfig(), "")
	if err != nil {
		t.Fatal(err)
	}
	if cp == w {
		t.Fatal("the two components rendered identical units")
	}
	if !strings.Contains(w, "<string>worker</string>") ||
		!strings.Contains(w, "<string>macos</string>") {
		t.Errorf("the worker unit does not run a labelled worker:\n%s", w)
	}
	if strings.Contains(cp, "<string>worker</string>") {
		t.Errorf("the control plane unit runs a worker:\n%s", cp)
	}
}

// TestCommandLineMatchesTheCLIGrammar pins the ordering that makes a generated
// unit actually start.
//
// Global flags come before the subcommand and component flags after it. Getting
// it backwards produces a unit that parses fine and fails at exec time, on a
// machine nobody is watching -- so it is checked here, where it is cheap.
func TestCommandLineMatchesTheCLIGrammar(t *testing.T) {
	argv := commandLine(workerConfig())
	want := []string{
		"/Users/you/.local/bin/je", "--data-dir", "/Users/you/.je",
		"worker", "run", "--addr", "127.0.0.1:7620", "--name", "macbook",
		"--labels", "macos",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %q, want %q", argv, want)
		}
	}
}
