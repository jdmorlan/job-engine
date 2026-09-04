package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/secretfile"
	"github.com/jdmorlan/job-engine/internal/secrets"
)

// IdentityFileName is where a worker keeps the age key that lets it read the
// secrets travelling with a source (D25).
const IdentityFileName = "identity"

// resolveSecrets decrypts the declared secrets the control plane could not
// supply, from the source tree this worker already has.
//
// This is C11 finished for secrets: the control plane sends names, and the
// process environment is built on the machine where the process will exist.
func (w *Worker) resolveSecrets(d engine.Dispatch, root string) (map[string]string, error) {
	if len(d.Secrets) == 0 {
		return nil, nil
	}
	// The same base resolveWorkdir uses: the source tree this job arrived with.
	// A secrets file lives beside the definitions it belongs to, so it is found
	// exactly where the code is (D22/D25).
	if root == "" {
		return nil, fmt.Errorf(
			"this job needs %s, which is encrypted in its source -- but the dispatch "+
				"carried no source tree to look in",
			strings.Join(d.Secrets, ", "))
	}

	id, err := w.identity()
	if err != nil {
		return nil, err
	}

	body, err := os.ReadFile(filepath.Join(root, secretfile.Name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(
			"this job needs %s, which the control plane does not hold -- and there is no %s beside its definitions",
			strings.Join(d.Secrets, ", "), secretfile.Name)
	}
	if err != nil {
		return nil, err
	}
	file, err := secretfile.Parse(body)
	if err != nil {
		return nil, err
	}
	all, err := file.Decrypt(id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", secretfile.Name, err)
	}

	// D10 unchanged, just relocated: only declared secrets are injected, so a
	// file holding twenty does not hand twenty to a job that asked for one.
	out := make(map[string]string, len(d.Secrets))
	for _, name := range d.Secrets {
		value, ok := all[name]
		if !ok {
			return nil, fmt.Errorf("%s does not contain %s", secretfile.Name, name)
		}
		out[name] = value
	}
	return out, nil
}

// identity loads this worker's age key.
//
// Read on every use rather than cached at startup, so rotating the key does not
// need a restart -- and so a worker that never runs a job needing secrets never
// reads it at all.
func (w *Worker) identity() (age.Identity, error) {
	path := w.opts.IdentityFile
	if path == "" {
		path = filepath.Join(w.opts.CacheDir, IdentityFileName)
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf(
			"this job's secrets are encrypted and this worker has no key at %s.\n"+
				"Create one with:  je worker keygen\n"+
				"then add the printed public key as a recipient of the source's %s",
			path, secretfile.Name)
	}
	if err != nil {
		return nil, err
	}
	ids, err := age.ParseIdentities(strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("reading the key at %s: %w", path, err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s contains no key", path)
	}
	return ids[0], nil
}

// redactorFor builds the replacer that strips worker-resolved secrets from log
// lines before they are shipped.
//
// The control plane redacts the values it holds, on the way into storage. It
// cannot redact these, because it cannot read them -- so this happens here, and
// earlier: the value is gone before the line crosses the network rather than
// after (D25).
func redactorFor(values map[string]string) *strings.Replacer {
	if len(values) == 0 {
		return nil
	}
	ordered := make([]string, 0, len(values))
	for _, v := range values {
		// Short values are left alone for the same reason the control plane
		// leaves them alone: redacting "1" would black out arithmetic.
		if len(v) >= secrets.MinRedactableLength {
			ordered = append(ordered, v)
		}
	}
	if len(ordered) == 0 {
		return nil
	}
	// Longest first, so a secret that contains another is replaced whole.
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })

	pairs := make([]string, 0, len(ordered)*2)
	for _, v := range ordered {
		pairs = append(pairs, v, "***")
	}
	return strings.NewReplacer(pairs...)
}
