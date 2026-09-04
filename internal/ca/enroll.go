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

	// bootstrap is the hash of the token written into the data directory for
	// workers on this machine. Empty until one is minted.
	bootstrap string
}

type pending struct {
	grant   Grant
	expires time.Time
}

// Grant is what a redeemed token entitles the holder to.
type Grant struct {
	// Worker and Labels are fixed by whoever minted the token, and are what the
	// enrolled identity becomes. Empty on a bootstrap grant.
	Worker string
	Labels []string

	// Roles are what this identity is for: executing work, or being a person
	// at a terminal. Fixed here for the same reason labels are -- a machine
	// that could decide it was a client could decide it may mint identities.
	//
	// Empty means the caller's default, which the engine fills in. This package
	// deliberately knows no role names: they are the store's vocabulary, and
	// the token store's job is only to carry what it was told.
	Roles []string

	// SelfNamed means the holder chooses its own name and labels.
	//
	// True only for a bootstrap token, which lives in the control plane's data
	// directory beside the CA private key. Anybody who can read it could read
	// that key and issue any certificate they liked, so requiring them to be
	// told a name as well would protect nothing -- it would only add a step to
	// the case that has no second party to protect anything from (D25).
	SelfNamed bool
}

func NewTokens() *Tokens { return &Tokens{issued: map[string]pending{}} }

// Issue mints a token that will enrol exactly one worker, under a name and
// labels chosen *here*.
//
// Both are fixed at issue time on purpose. Labels are capabilities and a worker
// advertising its own was the hole D25 names: declaring `macos` should not be
// something a machine can decide for itself once capabilities gate anything.
// Whoever mints the token decides what the machine is allowed to claim to be.
func (t *Tokens) Issue(worker string, labels, roles []string) (string, error) {
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
		grant: Grant{
			Worker: worker,
			Labels: append([]string(nil), labels...),
			Roles:  append([]string(nil), roles...),
		},
		expires: time.Now().Add(TokenLifetime),
	}
	return token, nil
}

// IssueBootstrap mints the token a control plane leaves in its own data
// directory for workers on the same machine (D25).
//
// Reusable, and long-lived for as long as this process is. Both would be wrong
// for a token that crosses a network and are unremarkable for one that does
// not: it never leaves a directory whose other contents include the key that
// signs everything, so it grants nothing that reading that directory did not
// already grant. Making it single-use would mean a worker restarting on the
// same machine needed a human, which is exactly the friction this removes.
func (t *Tokens) IssueBootstrap() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.bootstrap = hash(token)
	return token, nil
}

// Redeem consumes a token and returns what it was issued for.
//
// One use only: the token is deleted whether or not the enrolment that follows
// succeeds. A token that could be retried is a token that can be replayed, and
// a failed enrolment is cheap to reissue.
func (t *Tokens) Redeem(token string) (Grant, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked()

	key := hash(token)

	// The bootstrap token is not consumed: see IssueBootstrap. Compared in
	// constant time because it is the one token that stays valid, so a timing
	// difference here would be worth somebody measuring.
	if t.bootstrap != "" && subtle.ConstantTimeCompare([]byte(key), []byte(t.bootstrap)) == 1 {
		return Grant{SelfNamed: true}, nil
	}

	found, ok := t.issued[key]
	if !ok {
		return Grant{}, ErrBadToken
	}
	delete(t.issued, key)
	if time.Now().After(found.expires) {
		return Grant{}, ErrBadToken
	}
	return found.grant, nil
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
