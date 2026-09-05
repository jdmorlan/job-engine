package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jdmorlan/job-engine/internal/paths"
)

func upEnv(t *testing.T, version string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errs := &bytes.Buffer{}, &bytes.Buffer{}
	return &Env{
		Stdout:  out,
		Stderr:  errs,
		Stdin:   strings.NewReader(""),
		Layout:  paths.Layout{Data: t.TempDir()},
		Version: version,
	}, out, errs
}

// An explicit --docker or --native is an answer, and `je up` must not have
// opinions about a question it was already told the answer to.
func TestDefaultedModeLeavesAnExplicitChoiceAlone(t *testing.T) {
	env, _, errs := upEnv(t, "dev")

	for _, given := range []installMode{{docker: true}, {native: true}} {
		errs.Reset()
		if got := defaultedMode(env, given); got != given {
			t.Errorf("defaultedMode(%+v) = %+v, want it unchanged", given, got)
		}
		if errs.Len() != 0 {
			t.Errorf("explained a decision it did not make: %q", errs.String())
		}
	}
}

// The case every contributor hits first: their own build has no published
// image, so `je up` has to fall back to a native control plane rather than
// fail -- and has to say that it did.
func TestDefaultedModeFallsBackAudibly(t *testing.T) {
	env, _, errs := upEnv(t, "dev")

	got := defaultedMode(env, installMode{})
	if got.docker || !got.native {
		t.Fatalf("a dev build got %+v, want the native fallback", got)
	}
	said := errs.String()
	if said == "" {
		t.Fatal("fell back to a native control plane and said nothing")
	}
	for _, want := range []string{"native service", "je up --docker"} {
		if !strings.Contains(said, want) {
			t.Errorf("the explanation does not mention %q:\n%s", want, said)
		}
	}
}

// A build with no published image cannot run as a container whether or not
// Docker is installed, so this holds on any machine the tests run on.
func TestCanRunContainersRefusesADevBuild(t *testing.T) {
	env, _, _ := upEnv(t, "dev")
	ok, why := canRunContainers(env)
	if ok {
		t.Fatal("a dev build claims it can run as a container")
	}
	if why == "" {
		t.Error("refused without saying why")
	}
}

// The same property the tables have, for the one report that cannot use them:
// colour must not move anything. `je up` prints a line at a time so the reader
// sees progress, which means the columns are padded from a declared list
// rather than measured -- and padding a styled string by its byte length is
// exactly the bug that makes coloured output come apart.
func TestUpReportAlignsUnderColour(t *testing.T) {
	render := func(on bool) string {
		env, out, _ := upEnv(t, "dev")
		env.Style = Style{on: on}
		report := reporter(env, upComponents)
		report("control plane", env.Style.Good("already running"), "127.0.0.1:7620")
		report("worker", env.Style.Good("registered as a service"), "default")
		report("web", env.Style.Warn("not started"), "")
		return out.String()
	}

	plain, coloured := render(false), stripANSI(render(true))
	if plain != coloured {
		t.Errorf("colour changed the layout:\nplain:\n%s\ncoloured (stripped):\n%s",
			plain, coloured)
	}

	lines := strings.Split(strings.TrimSuffix(plain, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), plain)
	}
	// The detail column starts in the same place on every line that has one.
	first := strings.Index(lines[0], "127.0.0.1:7620")
	second := strings.Index(lines[1], "default")
	if first != second {
		t.Errorf("detail column is ragged: %d vs %d\n%s", first, second, plain)
	}
	// A line with no detail is not padded out to meet one.
	if strings.HasSuffix(lines[2], " ") {
		t.Errorf("trailing whitespace on %q", lines[2])
	}
}

// Every state these commands print has to be in upStates, or the column it
// sits in is padded to the wrong width.
func TestUpStatesCoverWhatIsPrinted(t *testing.T) {
	width := widest(upStates)
	for _, state := range upStates {
		if displayWidth(state) > width {
			t.Errorf("%q is wider than the column it is padded to", state)
		}
	}
	if width == 0 {
		t.Fatal("no states declared")
	}
}

func TestIndentIndentsEveryLine(t *testing.T) {
	got := indent("first\nsecond\n\nfourth\n", "  ")
	want := "  first\n  second\n\n  fourth"
	if got != want {
		t.Errorf("indent = %q, want %q", got, want)
	}
}

// quietly exists to hold a step's output, not to lose it.
func TestQuietlyCapturesBothStreams(t *testing.T) {
	env, out, errs := upEnv(t, "dev")

	c, err := quietly(env, func(sub *Env) error {
		sub.Stdout.Write([]byte("its own report\n"))
		sub.Stderr.Write([]byte("something worth keeping\n"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.out != "its own report\n" || c.notes != "something worth keeping\n" {
		t.Errorf("captured %+v", c)
	}
	if out.Len() != 0 || errs.Len() != 0 {
		t.Errorf("output leaked past the capture: %q / %q", out.String(), errs.String())
	}
}
