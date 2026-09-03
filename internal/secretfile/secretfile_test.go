package secretfile_test

import (
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jdmorlan/job-engine/internal/secretfile"
)

// The property the whole design rests on: the control plane can tell which
// secrets exist without being able to read any of them (D10/D25).
func TestNamesAreReadableWithoutAKey(t *testing.T) {
	writer, pub := keypair(t)
	f, err := secretfile.New([]string{pub})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Set(writer, "WEATHER_API_KEY", "hunter2"); err != nil {
		t.Fatal(err)
	}
	body, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	// Reparsed the way the control plane does: no key anywhere.
	parsed, err := secretfile.Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Names(); len(got) != 1 || got[0] != "WEATHER_API_KEY" {
		t.Errorf("Names() = %v, want [WEATHER_API_KEY]", got)
	}
	if !parsed.Has("WEATHER_API_KEY") {
		t.Error("Has() said no for a secret that is in the file")
	}
	if parsed.Has("NOPE") {
		t.Error("Has() said yes for a secret that is not")
	}

	// And the value is genuinely not in there in the clear.
	if strings.Contains(string(body), "hunter2") {
		t.Fatal("the plaintext value is present in the encrypted file")
	}
}

func TestARecipientCanReadAndAStrangerCannot(t *testing.T) {
	id, pub := keypair(t)
	f, err := secretfile.New([]string{pub})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Set(id, "TOKEN", "s3cret"); err != nil {
		t.Fatal(err)
	}

	got, err := f.Decrypt(id)
	if err != nil {
		t.Fatal(err)
	}
	if got["TOKEN"] != "s3cret" {
		t.Errorf("TOKEN = %q, want s3cret", got["TOKEN"])
	}

	stranger, _ := keypair(t)
	if _, err := f.Decrypt(stranger); err == nil {
		t.Fatal("a key that is not a recipient decrypted the file")
	} else if !strings.Contains(err.Error(), "recipients") {
		t.Errorf("error = %v, want it to say the key is not a recipient", err)
	}
}

// A value is bound to its own name, so moving ciphertext between keys fails
// rather than silently promoting a dev token to a production one.
func TestAValueCannotBeMovedToAnotherName(t *testing.T) {
	id, pub := keypair(t)
	f, _ := secretfile.New([]string{pub})
	if err := f.Set(id, "DEV_TOKEN", "dev"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set(id, "PROD_TOKEN", "prod"); err != nil {
		t.Fatal(err)
	}
	body, _ := f.Marshal()

	// Swap the two ciphertexts, the way an attacker with commit access would.
	swapped := strings.Replace(string(body), "DEV_TOKEN:", "TMP:", 1)
	swapped = strings.Replace(swapped, "PROD_TOKEN:", "DEV_TOKEN:", 1)
	swapped = strings.Replace(swapped, "TMP:", "PROD_TOKEN:", 1)

	tampered, err := secretfile.Parse([]byte(swapped))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tampered.Decrypt(id); err == nil {
		t.Fatal("ciphertext moved to a different name decrypted anyway")
	}
}

// Granting access requires having access, which is what keeps the recipient
// list an honest answer to "who can read this" (D25).
func TestAddingARecipientRequiresBeingOne(t *testing.T) {
	owner, ownerPub := keypair(t)
	f, _ := secretfile.New([]string{ownerPub})
	if err := f.Set(owner, "TOKEN", "s3cret"); err != nil {
		t.Fatal(err)
	}

	newcomer, newcomerPub := keypair(t)
	if _, err := f.Decrypt(newcomer); err == nil {
		t.Fatal("the newcomer could already read it")
	}

	stranger, _ := keypair(t)
	if err := f.AddRecipient(stranger, newcomerPub); err == nil {
		t.Fatal("a non-recipient granted access to somebody else")
	}

	if err := f.AddRecipient(owner, newcomerPub); err != nil {
		t.Fatal(err)
	}
	got, err := f.Decrypt(newcomer)
	if err != nil {
		t.Fatalf("the added recipient still cannot read: %v", err)
	}
	if got["TOKEN"] != "s3cret" {
		t.Errorf("TOKEN = %q, want s3cret", got["TOKEN"])
	}

	// The owner does not lose access by sharing it.
	if _, err := f.Decrypt(owner); err != nil {
		t.Errorf("the original recipient lost access: %v", err)
	}
}

// A file nobody can read is a mistake worth refusing at the point it is made.
func TestAFileNeedsARecipient(t *testing.T) {
	if _, err := secretfile.New(nil); err == nil {
		t.Fatal("created a secrets file with no recipients")
	}
}

func keypair(t *testing.T) (age.Identity, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id, id.Recipient().String()
}
