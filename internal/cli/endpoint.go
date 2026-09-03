package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Endpoint is where the control plane for this data directory lives.
//
// Written by `je control-plane install`, read by every command when there is no
// live runtime file to go on. See paths.Layout.Endpoint for why it exists: a
// containerised control plane publishes its runtime file inside its own volume,
// where nothing on the host can see it.
type Endpoint struct {
	Address string `json:"address"`

	// Kind is how it was set up -- "docker" or "native" -- so `remove` and
	// `status` can say something true rather than guessing.
	Kind string `json:"kind"`
}

// WriteEndpoint records where a control plane was put.
func WriteEndpoint(path string, e Endpoint) error {
	body, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Written to a temp file and renamed, so a client never reads a partial
	// one -- the same reason the runtime file does it.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".endpoint-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ReadEndpoint loads the recorded endpoint, if there is one.
func ReadEndpoint(path string) (Endpoint, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return Endpoint{}, err
	}
	var e Endpoint
	if err := json.Unmarshal(body, &e); err != nil {
		return Endpoint{}, fmt.Errorf("reading %s: %w", path, err)
	}
	return e, nil
}

// RemoveEndpoint forgets a recorded endpoint.
func RemoveEndpoint(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
