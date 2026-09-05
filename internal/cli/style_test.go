package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestDisplayWidthIgnoresEscapes is the test the alignment bug asked for.
//
// The first version of scanEscapes ended a CSI sequence at its '[', which made
// every coloured cell measure two characters short and every column two
// characters wide. Nothing failed: the tables were still tables, just visibly
// crooked, and only on a terminal.
func TestDisplayWidthIgnoresEscapes(t *testing.T) {
	on := Style{on: true}
	cases := []struct {
		name string
		text string
		want int
	}{
		{"plain", "succeeded", 9},
		{"dim", on.Muted("succeeded"), 9},
		{"bold", on.Title("je"), 2},
		{"nested colours", on.Good("ok") + " " + on.Muted("(dev)"), 8},
		{"empty", "", 0},
		{"styling an empty string adds nothing", on.Bad(""), 0},
		{"runes count once", "café", 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := displayWidth(c.text); got != c.want {
				t.Errorf("displayWidth(%q) = %d, want %d", c.text, got, c.want)
			}
			if got := stripANSI(c.text); displayWidth(got) != c.want || strings.Contains(got, "\x1b") {
				t.Errorf("stripANSI(%q) = %q", c.text, got)
			}
		})
	}
}

// TestTableAlignsColouredCells pins the property the whole style layer rests
// on: the same table, with and without colour, lines up in the same places.
func TestTableAlignsColouredCells(t *testing.T) {
	render := func(st Style) string {
		var buf bytes.Buffer
		tw := newTable(&buf)
		fmt.Fprintln(tw, st.Header("RUN\tJOB\tSTATUS"))
		fmt.Fprintf(tw, "%d\t%s\t%s\n", 1, "demo/demo-hello", st.State("succeeded"))
		fmt.Fprintf(tw, "%d\t%s\t%s\n", 22, "x", st.State("failed"))
		if err := tw.Flush(); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	plain := render(Style{})
	coloured := stripANSI(render(Style{on: true}))
	if plain != coloured {
		t.Errorf("colour changed the layout:\nplain:\n%s\ncoloured (stripped):\n%s",
			plain, coloured)
	}

	want := "RUN  JOB              STATUS\n" +
		"1    demo/demo-hello  succeeded\n" +
		"22   x                failed\n"
	if plain != want {
		t.Errorf("table = \n%q\nwant\n%q", plain, want)
	}
}

// TestTableLeavesNoTrailingSpaces: the last cell of a row is padded by
// nothing, so a line copied out of the terminal is the line it looked like.
func TestTableLeavesNoTrailingSpaces(t *testing.T) {
	var buf bytes.Buffer
	tw := newTable(&buf)
	fmt.Fprintf(tw, "a\tlong value here\n")
	fmt.Fprintf(tw, "bbbb\tx\n")
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("trailing whitespace on %q", line)
		}
	}
}

func TestResolveStyle(t *testing.T) {
	env := func(pairs map[string]string) func(string) string {
		return func(k string) string { return pairs[k] }
	}
	tty := map[string]string{"TERM": "xterm-256color"}

	cases := []struct {
		name string
		mode colorMode
		vars map[string]string
		want bool
	}{
		{"a buffer is never a screen", colorAuto, tty, false},
		{"--color=always overrides the buffer", colorAlways, tty, true},
		{"--color=never overrides everything", colorNever, tty, false},
		{"somebody who typed --color=always meant it, NO_COLOR or not", colorAlways,
			map[string]string{"TERM": "xterm", "NO_COLOR": "1"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveStyle(&bytes.Buffer{}, c.mode, env(c.vars))
			if got.Enabled() != c.want {
				t.Errorf("enabled = %v, want %v", got.Enabled(), c.want)
			}
		})
	}

	// The environment half, checked on its own: against a buffer every one of
	// these rules is unreachable, because a buffer is already not a terminal.
	t.Run("environment", func(t *testing.T) {
		envCases := []struct {
			name string
			vars map[string]string
			want bool
		}{
			{"a terminal", tty, true},
			{"NO_COLOR", map[string]string{"TERM": "xterm", "NO_COLOR": "1"}, false},
			{"an empty NO_COLOR is not set, per the convention",
				map[string]string{"TERM": "xterm", "NO_COLOR": ""}, true},
			{"TERM=dumb", map[string]string{"TERM": "dumb"}, false},
			{"no TERM at all", map[string]string{}, false},
		}
		for _, c := range envCases {
			if got := envAllowsColor(env(c.vars)); got != c.want {
				t.Errorf("%s: envAllowsColor = %v, want %v", c.name, got, c.want)
			}
		}
	})

	// os.DevNull is a real *os.File and not a terminal, which is the case that
	// catches a detector that only checks the concrete type.
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if resolveStyle(f, colorAuto, env(tty)).Enabled() {
		t.Error("colour enabled on a non-terminal file")
	}
}

// TestStyleOffIsIdentity: with colour off, nothing in the vocabulary may
// change a single byte. This is what lets every other test in the package
// assert on exact output.
func TestStyleOffIsIdentity(t *testing.T) {
	var off Style
	for _, f := range []func(string) string{
		off.Title, off.Header, off.Muted, off.Cmd, off.Good, off.Warn, off.Bad, off.State,
	} {
		if got := f("succeeded"); got != "succeeded" {
			t.Errorf("style off changed %q to %q", "succeeded", got)
		}
	}
}

// TestEveryCommandIsInAGroup: `je help` is the only complete list of what the
// engine can do, and a command that is registered and unlisted is invisible.
// The "other" section catches this at runtime; this catches it at review.
func TestEveryCommandIsInAGroup(t *testing.T) {
	grouped := map[string]bool{}
	for _, g := range groups {
		for _, name := range g.names {
			if grouped[name] {
				t.Errorf("%q is in two groups", name)
			}
			grouped[name] = true
			if _, ok := commands[name]; !ok {
				t.Errorf("group %q lists %q, which is not a registered command", g.title, name)
			}
		}
	}
	for name := range commands {
		if !grouped[name] {
			t.Errorf("command %q is in no group in `je help`; add it to one", name)
		}
	}
}
