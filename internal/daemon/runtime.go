package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultAddr is where the daemon listens unless told otherwise.
//
// Loopback, not 0.0.0.0: N1 keeps auth a non-goal, which is only defensible
// while the trust boundary is the machine. D19 later moves that boundary to a
// tailnet, which is a config change to this value and nothing else.
const DefaultAddr = "127.0.0.1:7620"

// RuntimeInfo is what a running daemon publishes about itself so that a CLI
// invocation can find it without being told (P3: the tool renders truth).
//
// It is written after the listener is bound, so the address in it is the one
// actually in use -- which matters when the configured port was 0, or when the
// daemon was started with a different --addr than the one you are assuming.
type RuntimeInfo struct {
	Address   string    `json:"address"`
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
}

// WriteRuntime publishes the daemon's runtime information atomically.
//
// Write-to-temp-then-rename, so a client never reads a half-written file. The
// window where the file exists but is empty is small, but it is exactly the
// window during which a CLI command is most likely to run: right after start.
func WriteRuntime(path string, info RuntimeInfo) error {
	body, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".daemon-*.json")
	if err != nil {
		return fmt.Errorf("creating runtime file: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds

	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadRuntime loads the runtime file. A missing file means no daemon has run
// from this data directory; os.IsNotExist on the error distinguishes that from
// a real failure.
//
// Note what this does NOT tell you: whether the daemon is still alive. The
// file survives a crash. Liveness is established by talking to the address,
// which the client does anyway -- so there is no point checking twice.
func ReadRuntime(path string) (RuntimeInfo, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return RuntimeInfo{}, err
	}
	var info RuntimeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return RuntimeInfo{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return info, nil
}
