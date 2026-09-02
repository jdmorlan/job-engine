package cli

import (
	"flag"
	"io"
	"testing"
)

// TestParseArgsAllowsInterleaving pins down the behaviour that the standard
// flag package does not give us: a flag after a positional argument is still a
// flag. `je emit homekit.motion --payload '{}'` is the shape people will
// actually type, and silently dropping the payload would be the worst kind of
// bug -- one where the command succeeds.
func TestParseArgsAllowsInterleaving(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantPayload string
		wantKey     string
		wantPos     []string
	}{
		{
			name:        "flags after positional",
			args:        []string{"homekit.motion", "--payload", `{"room":"office"}`},
			wantPayload: `{"room":"office"}`,
			wantPos:     []string{"homekit.motion"},
		},
		{
			name:        "flags before positional",
			args:        []string{"--payload", `{"a":1}`, "homekit.motion"},
			wantPayload: `{"a":1}`,
			wantPos:     []string{"homekit.motion"},
		},
		{
			name:        "flags on both sides",
			args:        []string{"--payload", "{}", "homekit.motion", "--dedupe-key", "k1"},
			wantPayload: "{}",
			wantKey:     "k1",
			wantPos:     []string{"homekit.motion"},
		},
		{
			name:    "positional only",
			args:    []string{"homekit.motion"},
			wantPos: []string{"homekit.motion"},
		},
		{
			name:    "several positionals",
			args:    []string{"one", "two", "three"},
			wantPos: []string{"one", "two", "three"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			payload := fs.String("payload", "", "")
			key := fs.String("dedupe-key", "", "")

			pos, err := parseArgs(fs, tc.args)
			if err != nil {
				t.Fatalf("parseArgs: %v", err)
			}
			if *payload != tc.wantPayload {
				t.Errorf("payload = %q, want %q", *payload, tc.wantPayload)
			}
			if *key != tc.wantKey {
				t.Errorf("dedupe-key = %q, want %q", *key, tc.wantKey)
			}
			if len(pos) != len(tc.wantPos) {
				t.Fatalf("positionals = %q, want %q", pos, tc.wantPos)
			}
			for i := range pos {
				if pos[i] != tc.wantPos[i] {
					t.Errorf("positional %d = %q, want %q", i, pos[i], tc.wantPos[i])
				}
			}
		})
	}
}

func TestParseArgsReportsUnknownFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	if _, err := parseArgs(fs, []string{"thing", "--nope"}); err == nil {
		t.Fatal("unknown flag accepted; expected a usage error")
	} else if _, ok := err.(usageError); !ok {
		t.Errorf("error is %T, want usageError (so the exit code is 2)", err)
	}
}
