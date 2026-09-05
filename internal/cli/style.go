package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Style is the CLI's visual vocabulary.
//
// The output was already saying the right things and still read as a wall:
// every character on the screen had the same weight, so the eye had nothing to
// land on and the reader had to parse a report they should have been able to
// scan. Colour here is not decoration, it is the difference between "a job
// failed" being a word in a table and being the thing you see first.
//
// It is a small, closed vocabulary on purpose. Six roles, chosen so that every
// caller has an obvious answer to "which one is this?", and so that a terminal
// with an unusual palette cannot turn the output into a ransom note. Nothing
// here ever carries meaning on its own: a failed run says "failed" in a colour,
// never only in a colour, because the same bytes go to a pipe, a CI log and a
// reader who cannot see red.
type Style struct {
	on bool
}

// ANSI select-graphic-rendition codes. Only the eight basic colours and the
// two intensity attributes: these are the ones every terminal, theme and
// remote session agrees on, and the bright/256-colour range is exactly where
// "unreadable on somebody's background" lives.
const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

func (s Style) wrap(code, text string) string {
	if !s.on || text == "" {
		return text
	}
	return code + text + ansiReset
}

// Title is the subject of a report: the job you asked about, the run you are
// watching. One per screen, near the top.
func (s Style) Title(text string) string { return s.wrap(ansiBold, text) }

// Header is a column heading or a section label -- the scaffolding that tells
// you what you are looking at, which is worth less ink than what it labels.
func (s Style) Header(text string) string { return s.wrap(ansiDim, text) }

// Muted is for what the reader needs available but not first: provenance,
// footnotes, "3 more hidden", the parenthetical after a value.
func (s Style) Muted(text string) string { return s.wrap(ansiDim, text) }

// Cmd marks text that is literally a command to type. It is the one thing in
// this output the reader might want to copy, so it gets its own colour and
// nothing else uses it.
func (s Style) Cmd(text string) string { return s.wrap(ansiCyan, text) }

// Good, Warn and Bad are states, not sentiment: succeeded, needs attention,
// failed. They stay off anything that is merely nice or unfortunate, so that
// a colour on this screen always means the engine is telling you something.
func (s Style) Good(text string) string { return s.wrap(ansiGreen, text) }
func (s Style) Warn(text string) string { return s.wrap(ansiYellow, text) }
func (s Style) Bad(text string) string  { return s.wrap(ansiRed, text) }

// Enabled reports whether styling is on, for the rare caller that wants to
// choose different words rather than a different colour.
func (s Style) Enabled() bool { return s.on }

// State colours one of the engine's status words.
//
// Centralised because the words appear in five different tables and the
// mapping is the kind of thing that drifts: "waiting" being yellow in `je
// runs` and green in `je waiting` would quietly teach the reader that the
// colours mean nothing. An unrecognised word is left alone rather than
// guessed at.
func (s Style) State(word string) string {
	switch word {
	case "succeeded", "ok", "online", "running", "healthy", "clean":
		return s.Good(word)
	case "failed", "load error", "misconfigured", "error", "offline", "broken":
		return s.Bad(word)
	case "cancelled", "timed out", "interrupted", "disabled", "skipped", "stale":
		return s.Warn(word)
	case "removed", "queued", "waiting", "pending":
		return s.Muted(word)
	default:
		return word
	}
}

// colorMode is the --color flag's value: what the user asked for, as opposed
// to what the terminal can do.
type colorMode string

const (
	colorAuto   colorMode = "auto"
	colorAlways colorMode = "always"
	colorNever  colorMode = "never"
)

// resolveStyle decides whether this particular stream gets colour.
//
// The order is deliberate. An explicit --color wins, because a person who
// typed it is answering this exact question. NO_COLOR comes next: it is the
// convention for "I have told every tool on this machine once, stop asking"
// (no-color.org), and a tool that overrides it on a guess is the reason the
// convention needed to exist. Everything after that is inference, and
// inference defaults to off: colour written into a pipe, a log file or a CI
// transcript is corruption of somebody's data, while colour missing from a
// terminal is merely plain.
func resolveStyle(w io.Writer, mode colorMode, getenv func(string) string) Style {
	switch mode {
	case colorAlways:
		return Style{on: true}
	case colorNever:
		return Style{on: false}
	}
	if !envAllowsColor(getenv) {
		return Style{on: false}
	}
	f, ok := w.(*os.File)
	if !ok {
		// A buffer, a pipe wrapper, a test. Not a screen.
		return Style{on: false}
	}
	return Style{on: term.IsTerminal(int(f.Fd()))}
}

// envAllowsColor is the half of the decision that does not depend on what is
// on the other end of the stream.
//
// Separate from resolveStyle so it can be tested for what it says rather than
// for what a test buffer happens to make true: every one of these rules is
// only reachable on a real terminal, and a test that checks them against a
// bytes.Buffer passes whether the rule is there or not.
func envAllowsColor(getenv func(string) string) bool {
	// NO_COLOR counts when it is present and non-empty, per the convention.
	if getenv("NO_COLOR") != "" {
		return false
	}
	// TERM=dumb is a terminal saying so itself, and an unset TERM usually
	// means no terminal at all.
	switch getenv("TERM") {
	case "dumb", "":
		return false
	}
	return true
}

// displayWidth is the width a string occupies on screen, which is not its
// length once it carries escape sequences.
//
// Every alignment decision in this package goes through here, because the
// alternative -- len() on a coloured cell -- produces a table that looks
// correct in tests and shreds itself the moment a colour is added. Counting
// runes rather than bytes for the same reason a job name may be in any
// language.
func displayWidth(s string) int {
	width := 0
	scanEscapes(s, func(r rune) { width++ })
	return width
}

// stripANSI removes escape sequences, for tests and for anything that needs
// the bare text back.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, escape) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	scanEscapes(s, func(r rune) { b.WriteRune(r) })
	return b.String()
}

const escape = '\x1b'

// scanEscapes calls visible for each rune that will appear on the screen,
// skipping ANSI escape sequences.
//
// Worth spelling out because getting it slightly wrong is silent: a CSI
// sequence is ESC, then '[', then parameter bytes, and only then a final byte
// in @ through ~. Treating the '[' itself as a terminator ends the sequence two
// bytes early, counts "2m" as visible text, and pads every coloured column two
// characters too far -- which reads as a table that has come loose rather than
// as a bug in a width function.
func scanEscapes(s string, visible func(rune)) {
	const (
		text = iota
		afterEscape
		inCSI
	)
	state := text
	for _, r := range s {
		switch state {
		case afterEscape:
			if r == '[' {
				state = inCSI
			} else {
				// A two-character escape: consumed whole.
				state = text
			}
		case inCSI:
			if r >= '@' && r <= '~' {
				state = text
			}
		default:
			if r == escape {
				state = afterEscape
				continue
			}
			visible(r)
		}
	}
}

// section prints a section heading, with an optional aside explaining what the
// section contains.
//
// Commands that print several lists in a row -- `je waiting` prints seven --
// are the ones most at risk of reading as one long paragraph, because the only
// thing separating the lists is a blank line and a word in capitals. One
// helper so that every such heading looks the same, and so that the aside
// after it is visibly an aside rather than part of the title.
func (e *Env) section(title, aside string) {
	line := e.Style.Header(title)
	if aside != "" {
		line += "  " + e.Style.Muted(aside)
	}
	fmt.Fprintln(e.Stdout, line)
}

// hint prints a line the reader can act on: a sentence, then the command that
// does it. The command is the only part they will copy, so it is the only part
// that gets a colour.
func (e *Env) hint(text, command string) {
	if text == "" {
		fmt.Fprintln(e.Stdout, "  "+e.Style.Cmd(command))
		return
	}
	fmt.Fprintln(e.Stdout, e.Style.Muted(text)+"  "+e.Style.Cmd(command))
}
