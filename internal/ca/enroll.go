package ca

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TokenLifetime is how long an enrolment token is good for.
//
// Short because a token is a bearer credential: whoever holds it becomes a
// worker. Fifteen minutes is enough to paste it into another terminal and not
// much else, which is the window it should have.
const TokenLifetime = 15 * time.Minute

// ErrBadToken is an enrolment attempt with a token that is wrong, used, or
// expired.
//
// One error for all three deliberately. Distinguishing "expired" from "never
// existed" tells somebody guessing tokens which guesses were close.
var ErrBadToken = errors.New("enrolment token is not valid")

// Tokens issues and redeems one-time enrolment tokens.
//
// Held in memory rather than the database, and that is a decision rather than
// laziness: a token is valid for minutes, so a control plane restart
// invalidating every outstanding one is a correct outcome, not a lost feature.
// Nothing durable means nothing to leak from a backup.
type Tokens struct {
	mu     sync.Mutex
	issued map[string]pending
}

type pending struct {
	worker  string
	labels  []string
	expires time.Time
}

func NewTokens() *Tokens { return &Tokens{issued: map[string]pending{}} }

// Issue mints a token that will enrol exactly one worker, under a name and
// labels chosen *here*.
//
// Both are fixed at issue time on purpose. Labels are capabilities and a worker
// advertising its own was the hole D25 names: declaring `macos` should not be
// something a machine can decide for itself once capabilities gate anything.
// Whoever mints the token decides what the machine is allowed to claim to be.
func (t *Tokens) Issue(worker string, labels []string) (string, error) {
	if worker == "" {
		return "", errors.New("an enrolment token needs a worker name")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	t.issued[hash(token)] = pending{
		worker:  worker,
		labels:  append([]string(nil), labels...),
		expires: time.Now().Add(TokenLifetime),
	}
	return token, nil
}

// Redeem consumes a token and returns what it was issued for.
//
// One use only: the token is deleted whether or not the enrolment that follows
// succeeds. A token that could be retried is a token that can be replayed, and
// a failed enrolment is cheap to reissue.
func (t *Tokens) Redeem(token string) (worker string, labels []string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()

	key := hash(token)
	found, ok := t.issued[key]
	if !ok {
		// Constant-time compared against nothing is still worth doing: the map
		// lookup already leaked, so the guard that matters is that every
		// failure returns the same error at the same cost.
		subtle.ConstantTimeCompare([]byte(key), []byte(key))
		return "", nil, ErrBadToken
	}
	delete(t.issued, key)
	if time.Now().After(found.expires) {
		return "", nil, ErrBadToken
	}
	return found.worker, found.labels, nil
}

// Outstanding reports how many tokens are live, for `je worker token --list`
// and for saying so in status. Not which ones: a token is a credential and the
// control plane should not be able to hand one back.
func (t *Tokens) Outstanding() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()
	return len(t.issued)
}

func (t *Tokens) sweepLocked() {
	now := time.Now()
	for key, p := range t.issued {
		if now.After(p.expires) {
			delete(t.issued, key)
		}
	}
}

// hash is what is stored, so the tokens map never holds a usable credential.
func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}
