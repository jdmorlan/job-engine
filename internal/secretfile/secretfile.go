// Package secretfile is the encrypted secrets that live in a source repository
// (D25).
//
// The shape is SOPS's, deliberately, and the reasons are not cosmetic:
//
//   - **Names are cleartext, values are encrypted.** The control plane can tell
//     that a declared secret exists without holding any key, so D10's rule --
//     a missing secret is a definition error at load time, not a cryptic exit
//     code at 3am -- survives secrets it cannot read.
//   - **One data key, encrypted to each recipient.** Rotating one secret changes
//     one line rather than a whole blob, and adding a recipient touches only the
//     metadata. Under D23 a human reviews these diffs, and "4KB of base64
//     changed" is not a review.
//
// It is not SOPS's *file format*: getsops/sops/v3/decrypt pulls 341 modules, 187
// of them cloud KMS SDKs this project will never call, against 13 for
// filippo.io/age. Wire compatibility buys only that the `sops` CLI can edit
// these files, and costs implementing their MAC and per-value AAD exactly. If it
// is ever wanted, it is a serialisation change behind this package.
package secretfile

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"filippo.io/age"
	"gopkg.in/yaml.v3"
)

// Name is what a secrets file is called inside a source tree.
const Name = "secrets.enc.yaml"

// File is a decrypted-or-not secrets file.
//
// Values are held encrypted until somebody asks for one, so simply loading a
// file -- which the control plane does on every sync -- never needs a key and
// never puts a secret in memory.
type File struct {
	// Recipients are the age public keys that can decrypt this file. Cleartext
	// on purpose: "who can read this" is the question an audit asks, and it
	// should be answerable by reading the file (D25).
	Recipients []string

	// dataKey, encrypted to each recipient. One key per file, so a value is
	// cheap to add and a recipient is cheap to change.
	wrappedKey string

	values map[string]string // name -> base64 ciphertext
}

type onDisk struct {
	Recipients []string          `yaml:"recipients"`
	DataKey    string            `yaml:"data_key"`
	Values     map[string]string `yaml:"values"`
}

// New returns an empty file readable by the given age recipients.
func New(recipients []string) (*File, error) {
	if len(recipients) == 0 {
		return nil, errors.New("a secrets file needs at least one recipient, " +
			"or nothing could ever read it")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	wrapped, err := wrap(key, recipients)
	if err != nil {
		return nil, err
	}
	return &File{
		Recipients: append([]string(nil), recipients...),
		wrappedKey: wrapped,
		values:     map[string]string{},
	}, nil
}

// Parse reads a secrets file. No key is required and none is used: this is the
// path the control plane takes, and it must not be able to do more than this.
func Parse(body []byte) (*File, error) {
	var raw onDisk
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("reading %s: %w", Name, err)
	}
	if raw.DataKey == "" {
		return nil, fmt.Errorf("%s has no data key: it was not written by je", Name)
	}
	if raw.Values == nil {
		raw.Values = map[string]string{}
	}
	return &File{Recipients: raw.Recipients, wrappedKey: raw.DataKey, values: raw.Values}, nil
}

// Names lists the secrets this file holds, without decrypting anything.
//
// This is what makes a declared-but-absent secret a load-time error for a
// control plane that cannot read the values (D10/D25).
func (f *File) Names() []string {
	out := make([]string, 0, len(f.values))
	for name := range f.values {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Has reports whether a named secret is present.
func (f *File) Has(name string) bool { _, ok := f.values[name]; return ok }

// Marshal renders the file for writing into a repository.
func (f *File) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("# Encrypted by `je secret set`. Values are unreadable without a\n" +
		"# recipient's key; names are deliberately not, so the engine can tell a\n" +
		"# declared secret is missing without being able to read any (D25).\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(onDisk{
		Recipients: f.Recipients,
		DataKey:    f.wrappedKey,
		Values:     f.values,
	}); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Set adds or replaces one secret. The caller must hold an identity that can
// unwrap the data key -- you cannot write into a file you could not read, which
// is what keeps "who has access" answerable from the recipient list alone.
func (f *File) Set(id age.Identity, name, value string) error {
	if name == "" {
		return errors.New("a secret needs a name")
	}
	key, err := f.unwrap(id)
	if err != nil {
		return err
	}
	sealed, err := seal(key, name, []byte(value))
	if err != nil {
		return err
	}
	f.values[name] = sealed
	return nil
}

// Remove deletes a secret from the file.
//
// Callers must not present this as revocation: the old ciphertext is in the
// repository's history forever, and anybody with that commit and a key can still
// read it. Removing is tidying; rotating is the security operation (D25).
func (f *File) Remove(name string) { delete(f.values, name) }

// Decrypt returns every secret in the file. This is the worker's path: it holds
// an identity, it built the process environment, and it needs the values.
func (f *File) Decrypt(id age.Identity) (map[string]string, error) {
	key, err := f.unwrap(id)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(f.values))
	for name, sealed := range f.values {
		plain, err := open(key, name, sealed)
		if err != nil {
			return nil, fmt.Errorf("decrypting %s: %w", name, err)
		}
		out[name] = string(plain)
	}
	return out, nil
}

// AddRecipient rewraps the data key so another identity can read the file.
//
// Requires a key that can already read it: access is granted by somebody who
// has access, never by the control plane on its own.
func (f *File) AddRecipient(id age.Identity, recipient string) error {
	key, err := f.unwrap(id)
	if err != nil {
		return err
	}
	for _, existing := range f.Recipients {
		if existing == recipient {
			return nil
		}
	}
	next := append(append([]string(nil), f.Recipients...), recipient)
	wrapped, err := wrap(key, next)
	if err != nil {
		return err
	}
	f.Recipients, f.wrappedKey = next, wrapped
	return nil
}

func (f *File) unwrap(id age.Identity) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(f.wrappedKey)
	if err != nil {
		return nil, fmt.Errorf("the data key is not valid base64: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(raw), id)
	if err != nil {
		return nil, fmt.Errorf("this key cannot read %s -- it is not one of its "+
			"%d recipients", Name, len(f.Recipients))
	}
	return io.ReadAll(r)
}

func wrap(key []byte, recipients []string) (string, error) {
	parsed := make([]age.Recipient, 0, len(recipients))
	for _, r := range recipients {
		p, err := age.ParseX25519Recipient(strings.TrimSpace(r))
		if err != nil {
			return "", fmt.Errorf("recipient %q is not an age public key: %w", r, err)
		}
		parsed = append(parsed, p)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, parsed...)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(key); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// seal encrypts one value, binding it to its own name.
//
// The name is additional authenticated data, so a value cannot be moved to
// another key by editing the file: swapping DEV_TOKEN's ciphertext onto
// PROD_TOKEN fails to open rather than silently succeeding.
func seal(key []byte, name string, plain []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plain, []byte(name))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func open(key []byte, name, encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("the value is too short to be a ciphertext")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	return gcm.Open(nil, nonce, body, []byte(name))
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
