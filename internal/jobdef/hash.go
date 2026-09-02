package jobdef

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Snapshot renders the definition as the canonical JSON that gets stored in
// job_versions and hashed (D11).
//
// It contains the *effective* definition -- declared values and defaults
// together -- for a specific reason. D11 exists so a run's detail view can show
// the exact definition it executed under. A snapshot recording only what the
// author wrote could not answer "what timeout was in force?" after the engine's
// default changed, which is precisely the question the feature is for.
//
// The visible consequence to accept: upgrading the engine in a way that changes
// a default creates a new definition version for every affected job. That is
// honest -- those jobs really did start running under different effective
// config -- and it shows up as a diff in the run detail rather than as silence.
func (d *Definition) Snapshot() ([]byte, error) {
	// encoding/json sorts struct fields by declaration order and map keys
	// lexically, and Definition has no maps, so marshalling is deterministic
	// without a canonicalisation pass. The test in hash_test.go pins that.
	body, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("snapshotting %s: %w", d.Slug, err)
	}
	return body, nil
}

// Hash is the definition's identity for D11: stable across loads, different the
// moment any effective value changes.
func (d *Definition) Hash() (string, error) {
	body, err := d.Snapshot()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	// Half a SHA-256 is 128 bits, which is plenty to make a collision a
	// non-event, and it stays short enough to print in a run detail view.
	return hex.EncodeToString(sum[:16]), nil
}

// FromSnapshot reconstructs a definition from its stored JSON.
//
// This is what makes D11 real rather than decorative: a run records the hash it
// executed under, and the definition behind that hash can be read back exactly,
// long after the file has changed. Unexported fields -- the declared-line map
// and the file path -- do not survive, because they describe the file rather
// than the job.
func FromSnapshot(body []byte) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("reading definition snapshot: %w", err)
	}
	return &d, nil
}
