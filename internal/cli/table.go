package cli

import (
	"bytes"
	"io"
	"strings"
)

// table aligns tab-separated rows into columns.
//
// This exists because text/tabwriter measures a cell with len(), and a cell
// that carries a colour is several bytes wider than it looks. Feeding styled
// text to tabwriter produces a table that is correct in a test buffer and
// visibly ragged on the screen the moment anything is coloured -- the exact
// failure that makes people give up on colouring tables at all.
//
// It is a deliberate reimplementation rather than a wrapper: the whole of
// tabwriter that this package used was "pad every column to its widest cell,
// two spaces between", which is thirty lines once width is measured properly.
// Interface-compatible with what it replaced (write tab-separated lines, then
// Flush) so that colouring a table is a change to one Fprintf and not to the
// shape of the command.
type table struct {
	w   io.Writer
	buf bytes.Buffer
	pad int
}

// newTable returns a table writing to w, with the two-space column gap the
// engine's output has always used.
func newTable(w io.Writer) *table { return &table{w: w, pad: 2} }

// table returns a table on the command's stdout.
func (e *Env) table() *table { return newTable(e.Stdout) }

// Write buffers. Nothing can be aligned until every row has been seen, which
// is why a table must be flushed and why Flush's error is worth returning.
func (t *table) Write(p []byte) (int, error) { return t.buf.Write(p) }

// Flush writes the aligned table.
//
// Column widths are computed across the whole table. No line ends in
// whitespace: an empty last cell would otherwise leave the padding of the cell
// before it hanging off the end, which is invisible until somebody copies the
// line out of their terminal and then it is a diff.
func (t *table) Flush() error {
	text := t.buf.String()
	t.buf.Reset()
	if text == "" {
		return nil
	}

	// A trailing newline would otherwise become a final empty row.
	trailing := strings.HasSuffix(text, "\n")
	if trailing {
		text = text[:len(text)-1]
	}

	var rows [][]string
	for _, line := range strings.Split(text, "\n") {
		rows = append(rows, strings.Split(line, "\t"))
	}

	// The width of each column, ignoring each row's last cell: a cell with
	// nothing to its right does not have to line up with anything.
	var widths []int
	for _, row := range rows {
		for i := 0; i < len(row)-1; i++ {
			for len(widths) <= i {
				widths = append(widths, 0)
			}
			if w := displayWidth(row[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}

	var out, line strings.Builder
	for _, row := range rows {
		line.Reset()
		for i, cell := range row {
			line.WriteString(cell)
			if i == len(row)-1 {
				break
			}
			line.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+t.pad))
		}
		out.WriteString(strings.TrimRight(line.String(), " "))
		out.WriteString("\n")
	}

	s := out.String()
	if !trailing {
		s = strings.TrimSuffix(s, "\n")
	}
	_, err := io.WriteString(t.w, s)
	return err
}
