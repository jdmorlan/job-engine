package api

import (
	"context"
	"net/http"
)

// identityKey is where a verified client certificate's worker name is kept for
// the duration of a request.
type identityKey struct{}

// WithIdentity records the worker a request proved it is.
//
// Set only from a certificate this control plane's own authority signed, which
// the TLS stack has already verified by the time a handler runs. Nothing a
// client sends in a body or a header can put a value here (D25).
func WithIdentity(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, identityKey{}, name)
}

// IdentityOf returns the worker a request proved it is, or "" for a request
// that presented no certificate.
//
// Empty is the ordinary case, not an error: the CLI and the web client are
// clients too and have no identity, and read endpoints do not need one. It
// matters only where a worker is acting as itself.
func IdentityOf(ctx context.Context) string {
	name, _ := ctx.Value(identityKey{}).(string)
	return name
}

// withClientIdentity lifts a verified client certificate into the request
// context.
//
// The common name is the identity, and it is taken from the *verified* chain
// rather than from the raw peer certificates -- so a self-signed certificate
// claiming CN=macbook contributes nothing, because it never verified.
func withClientIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
			leaf := r.TLS.VerifiedChains[0][0]
			if name := leaf.Subject.CommonName; name != "" {
				r = r.WithContext(WithIdentity(r.Context(), name))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// actorOf decides who is responsible for a request, preferring what was proved
// over what was claimed (D7/D25).
//
// `RunOptions.Actor` is described as "the person responsible", and it used to
// arrive in the request body -- which made it exactly the kind of assertion
// D25 removed from a worker's name. A verified certificate's common name cannot
// be asked for on somebody else's behalf, so when there is one it is the answer
// and the body is not consulted.
//
// The claimed value survives only where nothing was proved. That combination is
// reachable only before a deployment issues its first client identity; after
// that the write never gets this far.
func actorOf(r *http.Request, claimed string) string {
	if name := IdentityOf(r.Context()); name != "" {
		return name
	}
	return claimed
}
