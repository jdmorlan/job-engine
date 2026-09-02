package engine

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/secrets"
)

// The secret surface the API exposes.
//
// D15's rule is that every capability is an endpoint, so there is exactly one
// code path into the engine's state. `je secret` used to be the one command
// that broke it: it opened the on-disk store directly, which was invisible on a
// laptop and silently wrong the moment the control plane was somewhere else --
// you would set a secret on your Mac and wonder why the job could not see it.
//
// D20 makes that failure the normal case rather than an edge case, since the
// control plane is a container even during development.
//
// There is still no way to read a value back (D10). Set, list metadata, delete.

// SecretUse is one stored secret and the jobs that declare it.
type SecretUse struct {
	Name  string    `json:"name"`
	SetAt time.Time `json:"set_at"`

	// Jobs are the slugs declaring this secret. It answers "what breaks if I
	// rotate this?", which is the question a bare list cannot.
	Jobs []string `json:"jobs"`
}

// SecretsView is everything `je secret list` renders.
//
// The join between secrets and the jobs declaring them happens here rather than
// in the CLI, because the CLI is a renderer (D20): it should not have to fetch
// two collections and know how they relate.
type SecretsView struct {
	Secrets []SecretUse `json:"secrets"`

	// Unset are secrets a job declares that are not set. This is the inverse
	// view and the one that actually unblocks somebody, so it is part of the
	// response rather than something a client is expected to derive.
	Unset []SecretUse `json:"unset"`

	// DirectoryPrivate reports whether the data directory is readable by other
	// users on the machine hosting the control plane -- which, now that the
	// control plane may be a container somewhere else, is not a question the
	// CLI can answer for itself.
	DirectoryPrivate bool `json:"directory_private"`
}

// SetSecretResult reports what a client should warn about after storing one.
//
// The warnings are computed where the facts are (D10, P1): both concern the
// control plane's own filesystem, and only it can see them.
type SetSecretResult struct {
	Name string `json:"name"`

	// Redactable is false for a value too short to strip from logs safely.
	Redactable bool `json:"redactable"`

	// DirectoryPrivate is false when other users on the control plane's
	// machine can read its data directory.
	DirectoryPrivate bool `json:"directory_private"`
}

// SetSecret stores a value.
func (e *Engine) SetSecret(ctx context.Context, name, value string) (SetSecretResult, error) {
	if !secrets.ValidName(name) {
		return SetSecretResult{}, fmt.Errorf(
			"%q is not a valid secret name; use A-Z, digits and underscores", name)
	}
	if value == "" {
		return SetSecretResult{}, fmt.Errorf("refusing to store an empty value for %s", name)
	}
	if err := e.secrets.Set(name, value); err != nil {
		return SetSecretResult{}, err
	}
	private, err := e.secrets.DirectoryIsPrivate()
	if err != nil {
		// Not knowing is not a reason to fail a write that already succeeded.
		private = true
	}
	return SetSecretResult{
		Name:             name,
		Redactable:       len(value) >= secrets.MinRedactableLength,
		DirectoryPrivate: private,
	}, nil
}

// DeleteSecret removes a value.
func (e *Engine) DeleteSecret(ctx context.Context, name string) error {
	return e.secrets.Delete(name)
}

// SecretsView lists stored secrets and joins them to the jobs declaring them.
func (e *Engine) SecretsView(ctx context.Context) (SecretsView, error) {
	entries, err := e.secrets.List()
	if err != nil {
		return SecretsView{}, err
	}
	known := make(map[string]bool, len(entries))
	for _, entry := range entries {
		known[entry.Name] = true
	}

	jobs, err := e.Jobs(ctx)
	if err != nil {
		return SecretsView{}, err
	}

	users := map[string][]string{}
	unset := map[string][]string{}
	for _, job := range jobs {
		def, err := jobdef.FromSnapshot(job.Definition)
		if err != nil {
			// A job whose snapshot will not parse is already reported as
			// misconfigured by `je jobs`; it must not break the secret list.
			continue
		}
		for _, name := range def.Secrets {
			if known[name] {
				users[name] = append(users[name], job.Slug)
			} else {
				unset[name] = append(unset[name], job.Slug)
			}
		}
	}

	view := SecretsView{DirectoryPrivate: true}
	if private, err := e.secrets.DirectoryIsPrivate(); err == nil {
		view.DirectoryPrivate = private
	}
	for _, entry := range entries {
		view.Secrets = append(view.Secrets, SecretUse{
			Name: entry.Name, SetAt: entry.SetAt, Jobs: users[entry.Name],
		})
	}
	for name, slugs := range unset {
		view.Unset = append(view.Unset, SecretUse{Name: name, Jobs: slugs})
	}
	sort.Slice(view.Unset, func(i, j int) bool { return view.Unset[i].Name < view.Unset[j].Name })
	return view, nil
}
