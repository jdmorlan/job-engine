package jobdef

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration so it reads and writes as "15m" in both YAML and
// JSON, rather than as a nanosecond integer.
//
// The wrapper exists because the definition is round-tripped through JSON for
// hashing and snapshotting (D11), and a snapshot that says 3600000000000 where
// the file said 1h is a snapshot nobody can read at 2am.
type Duration struct{ D time.Duration }

// String renders the duration the way a file would write it: "30m", not
// "30m0s".
//
// This is the stored and hashed form as well as the displayed one, and that is
// deliberate -- the wrapper exists so a snapshot reads the way the file did,
// and trailing zero units are exactly the kind of noise it was added to
// prevent. Values round-trip either way; only the text changes.
func (d Duration) String() string {
	if d.D == 0 {
		return ""
	}
	// Longest first: "1h0m0s" also ends in "m0s", and trimming that leaves
	// "1h0m".
	text := d.D.String()
	switch {
	case strings.HasSuffix(text, "h0m0s"):
		return strings.TrimSuffix(text, "0m0s")
	case strings.HasSuffix(text, "m0s"):
		return strings.TrimSuffix(text, "0s")
	default:
		return text
	}
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	return d.parse(s)
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("line %d: duration must be a string like \"15m\"", node.Line)
	}
	if err := d.parse(s); err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	return nil
}

func (d *Duration) parse(s string) error {
	if s == "" {
		d.D = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		// time.ParseDuration's own message mentions Go syntax, which is not
		// what the author wrote or thinks they wrote.
		return fmt.Errorf("%q is not a duration; use forms like 30s, 15m, 4h", s)
	}
	d.D = parsed
	return nil
}
