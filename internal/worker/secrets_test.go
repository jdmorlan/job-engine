package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/secretfile"
)

// C11 finished for secrets: the control plane sends names, and the value is
// injected on the machine where the process will exist (D25).
func TestTheWorkerDecryptsTheSecretsItWasOnlyNamed(t *testing.T) {
	w, tree := workerWithKey(t, map[string]string{
		"WEATHER_API_KEY": "sk-live-value",
		"UNRELATED":       "not-for-this-job",
	})

	got, err := w.resolveSecrets(engine.Dispatch{Secrets: []string{"WEATHER_API_KEY"}}, tree)
	if err != nil {
		t.Fatal(err)
	}
	if got["WEATHER_API_KEY"] != "sk-live-value" {
		t.Errorf("WEATHER_API_KEY = %q, want the decrypted value", got["WEATHER_API_KEY"])
	}
	// D10 relocated, not relaxed: a file holding two secrets hands over only
	// the one that was declared.
	if _, leaked := got["UNRELATED"]; leaked {
		t.Error("a secret the job did not declare was injected anyway")
	}
}

// A worker with no key must say so, and name the fix. The alternative is a job
// running without its credentials and failing with whatever the command prints.
func TestAWorkerWithoutAKeySaysSo(t *testing.T) {
	w, tree := workerWithKey(t, map[string]string{"TOKEN": "v"})
	if err := os.Remove(w.opts.IdentityFile); err != nil {
		t.Fatal(err)
	}

	_, err := w.resolveSecrets(engine.Dispatch{Secrets: []string{"TOKEN"}}, tree)
	if err == nil {
		t.Fatal("a worker with no key resolved a secret")
	}
	if !strings.Contains(err.Error(), "je worker keygen") {
		t.Errorf("error = %v, want it to name the command that fixes this", err)
	}
}

// A key that is not a recipient is the ordinary mistake -- you generated a key
// and have not been added yet -- so it has to read as that rather than as
// corruption.
func TestAKeyThatIsNotARecipientIsRefused(t *testing.T) {
	w, tree := workerWithKey(t, map[string]string{"TOKEN": "v"})

	stranger, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.opts.IdentityFile, []byte(stranger.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = w.resolveSecrets(engine.Dispatch{Secrets: []string{"TOKEN"}}, tree)
	if err == nil {
		t.Fatal("a key that is not a recipient decrypted the file")
	}
	if !strings.Contains(err.Error(), "recipients") {
		t.Errorf("error = %v, want it to say this key is not a recipient", err)
	}
}

// A declared secret that is in neither the store nor the file must not run the
// job without it.
func TestASecretMissingFromTheFileIsRefused(t *testing.T) {
	w, tree := workerWithKey(t, map[string]string{"TOKEN": "v"})

	_, err := w.resolveSecrets(engine.Dispatch{Secrets: []string{"SOMETHING_ELSE"}}, tree)
	if err == nil {
		t.Fatal("a job ran without a secret it declared")
	}
	if !strings.Contains(err.Error(), "SOMETHING_ELSE") {
		t.Errorf("error = %v, want it to name the missing secret", err)
	}
}

// Redaction happens here because it cannot happen anywhere else: the control
// plane never sees these values (D25).
func TestWorkerRedactionStripsValuesLongestFirst(t *testing.T) {
	r := redactorFor(map[string]string{
		"SHORT": "ab", // below MinRedactableLength, deliberately left alone
		"TOKEN": "sk-live-abcdefghijkl",
		"INNER": "sk-live-abcdef",
	})
	if r == nil {
		t.Fatal("no redactor was built for values that need one")
	}
	got := r.Replace("using sk-live-abcdefghijkl and sk-live-abcdef now")
	if strings.Contains(got, "sk-live") {
		t.Errorf("redacted = %q, want no secret material left", got)
	}
	if got != "using *** and *** now" {
		t.Errorf("redacted = %q, want both replaced whole", got)
	}
}

// workerWithKey returns a worker holding a key that can read a secrets file
// written into the tree it is given.
func workerWithKey(t *testing.T, values map[string]string) (*Worker, string) {
	t.Helper()

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	f, err := secretfile.New([]string{id.Recipient().String()})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range values {
		if err := f.Set(id, name, value); err != nil {
			t.Fatal(err)
		}
	}
	body, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, secretfile.Name), body, 0o600); err != nil {
		t.Fatal(err)
	}

	data := t.TempDir()
	identity := filepath.Join(data, IdentityFileName)
	if err := os.WriteFile(identity, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return &Worker{opts: Options{
		Name: "test", JobsDir: tree, CacheDir: data, IdentityFile: identity,
	}}, tree
}
