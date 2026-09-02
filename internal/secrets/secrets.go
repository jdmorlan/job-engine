// Package secrets stores the values jobs declare and the engine injects.
//
// D10's framing is that the hard part is not storage, it is *when you find out
// something is wrong*. So the design goal here is a pit of success: nobody
// hand-edits a file, nobody pastes a token into shell history, a missing secret
// is a load-time error rather than a cryptic exit code at 3am, and the CLI has
// no command that prints a value.
//
// Storage is a mode-0600 JSON file in the engine's data directory, which is
// itself 0700. That is deliberately unglamorous: it is not encryption, and the
// threat it addresses is "another user on this machine" and "I accidentally
// committed my tokens", not a determined local attacker. An OS keychain backend
// is a plausible v2 and slots in behind these same functions.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// ErrNotFound means no secret is stored under that name.
var ErrNotFound = errors.New("secret is not set")

// MinRedactableLength is the shortest value the log redactor will strip.
//
// Below this, replacing every occurrence would mangle unrelated output -- a
// two-character secret would blank out fragments of ordinary words. Short
// values are still stored and injected; they are simply not redactable, and
// `je secret set` says so at the moment you set one rather than leaving you to
// discover it in a log.
const MinRedactableLength = 4

// namePattern is what a secret may be called. It must be a legal environment
// variable name, since that is how it reaches the job.
var namePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ValidName reports whether a name can be used.
func ValidName(name string) bool { return namePattern.MatchString(name) }

// Entry is one stored secret's metadata. The value is deliberately not here --
// listing secrets must never risk rendering one.
type Entry struct {
	Name  string    `json:"-"`
	SetAt time.Time `json:"set_at"`
}

// Store is the on-disk secret store.
type Store struct{ path string }

// Open returns a store backed by a file in the data directory. The file need
// not exist yet.
func Open(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, "secrets.json")}
}

// Path reports where the store lives, for messages that need to tell a human
// where their tokens are.
func (s *Store) Path() string { return s.path }

type file struct {
	Version int                     `json:"version"`
	Secrets map[string]storedSecret `json:"secrets"`
}

type storedSecret struct {
	Value string    `json:"value"`
	SetAt time.Time `json:"set_at"`
}

func (s *Store) load() (file, error) {
	body, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return file{Version: 1, Secrets: map[string]storedSecret{}}, nil
	} else if err != nil {
		return file{}, fmt.Errorf("reading secret store: %w", err)
	}

	var f file
	if err := json.Unmarshal(body, &f); err != nil {
		return file{}, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	if f.Secrets == nil {
		f.Secrets = map[string]storedSecret{}
	}
	return f, nil
}

func (s *Store) save(f file) error {
	f.Version = 1
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}

	// Written to a temp file in the same directory and renamed, so a crash
	// mid-write cannot leave a truncated store -- losing every secret at once
	// is a bad way to find out the write was not atomic.
	//
	// CreateTemp makes the file 0600 already, which matters: the window where
	// a world-readable temp file holds every token must not exist at all.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*.json")
	if err != nil {
		return fmt.Errorf("writing secret store: %w", err)
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	// Defensive: CreateTemp already makes 0600, but an existing store could
	// have been created by an older build or loosened by hand, and rename
	// preserves the source file's mode rather than fixing the destination's.
	return os.Chmod(s.path, 0o600)
}

// DirectoryIsPrivate reports whether the store's directory is readable only by
// its owner.
//
// The secret file itself is always 0600, so this is not about the tokens --
// it is about everything beside them. A data directory the rest of the machine
// can list also exposes the run logs, which routinely contain more than you
// think. The engine creates it 0700; a directory that pre-existed keeps
// whatever mode it was made with, and this is how a human finds that out.
func (s *Store) DirectoryIsPrivate() (bool, error) {
	info, err := os.Stat(filepath.Dir(s.path))
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 == 0, nil
}

// Set stores a value, replacing any previous one.
func (s *Store) Set(name, value string) error {
	if !ValidName(name) {
		return fmt.Errorf("%q is not a valid secret name; use A-Z, digits and underscores", name)
	}
	if value == "" {
		return errors.New("refusing to store an empty secret")
	}

	f, err := s.load()
	if err != nil {
		return err
	}
	f.Secrets[name] = storedSecret{Value: value, SetAt: time.Now()}
	return s.save(f)
}

// Get returns a value. Used only by the engine when building a job's
// environment; there is no CLI command that reaches this (D10).
func (s *Store) Get(name string) (string, error) {
	f, err := s.load()
	if err != nil {
		return "", err
	}
	entry, ok := f.Secrets[name]
	if !ok {
		return "", fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return entry.Value, nil
}

// Delete removes a secret. Removing one that is not there is not an error.
func (s *Store) Delete(name string) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	delete(f.Secrets, name)
	return s.save(f)
}

// Names returns every stored secret name, sorted.
func (s *Store) Names() ([]string, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(f.Secrets))
	for name := range f.Secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// List returns metadata for every stored secret, sorted by name.
func (s *Store) List() ([]Entry, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(f.Secrets))
	for name, entry := range f.Secrets {
		out = append(out, Entry{Name: name, SetAt: entry.SetAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Missing returns the declared names that are not set, preserving order.
//
// This is what turns a missing secret into a load-time definition error rather
// than a runtime surprise -- the single most valuable thing in D10.
func (s *Store) Missing(declared []string) ([]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range declared {
		if _, ok := f.Secrets[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// Resolve returns name=value pairs for the declared secrets, for injection.
//
// Only declared secrets are returned: a job does not receive the whole store,
// so adding a token for one job cannot silently widen what every other job can
// read.
func (s *Store) Resolve(declared []string) (map[string]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(declared))
	for _, name := range declared {
		entry, ok := f.Secrets[name]
		if !ok {
			return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		out[name] = entry.Value
	}
	return out, nil
}
