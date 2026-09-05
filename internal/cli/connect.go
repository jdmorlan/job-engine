package cli

import (
	"context"
	"errors"
	"fmt"
)

// withClient runs fn against the control plane.
//
// D20/C11 leaves exactly one way for the CLI to see anything: ask the control
// plane. There is no second implementation and no fallback to opening the
// database here.
//
// The fallback that used to live here is worth recording, because the reason it
// was wrong is not obvious. It opened the engine in-process whenever the control
// plane was unreachable, which meant `je runs` cheerfully printed history while
// nothing had been scheduled since midnight -- the exact 2am confusion P1 exists
// to prevent, produced by the tool whose whole claim is that it can be debugged.
// It also answered "what is this job?" from disk while a running control plane
// answered from what it had loaded, so the same question had two answers
// depending on something invisible.
//
// It bought one saved command (`je up --foreground`) and cost a second implementation of
// every read. See v0.6's rule: a capability is not a reason.
func withClient(ctx context.Context, env *Env, fn func(context.Context, *Client) error) error {
	client, err := connectOrAdvise(ctx, env)
	if err != nil {
		return err
	}
	return fn(ctx, client)
}

// connectOrAdvise is withClient without the callback, for the one command that
// wants to do something about a missing control plane rather than report it.
func connectOrAdvise(ctx context.Context, env *Env) (*Client, error) {
	client, err := Connect(env.Layout)
	if err != nil {
		return nil, adviseNoControlPlane(env, err)
	}
	if !reachable(ctx, client) {
		return nil, adviseNoControlPlane(env, fmt.Errorf("%w at %s", ErrNoControlPlane, client.base.Host))
	}
	return client, nil
}

// reachable reports whether the control plane actually answers, as opposed to
// having left a runtime file behind when it died.
func reachable(ctx context.Context, c *Client) bool {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := c.Health(ctx)
	return err == nil
}

// adviseNoControlPlane turns "cannot reach it" into "here is how to start one".
//
// P1: this sentence is worth more than the run list it replaces, because it is
// true. The old fallback's answer looked more helpful and was misleading.
//
// Only for the error it is advice about. Connecting can now fail for reasons
// that are not "there isn't one" -- no authority on this machine, or a control
// plane too old to speak TLS (D25) -- and telling somebody to start a control
// plane while one is running and answering is the same class of confidently
// wrong sentence this function exists to replace.
func adviseNoControlPlane(env *Env, err error) error {
	if !errors.Is(err, ErrNoControlPlane) {
		return err
	}
	// `je control-plane run` is deliberately not the headline. It starts a
	// control plane with no worker, which runs nothing (C11) -- suggesting it
	// first sends somebody to a second, quieter failure, which is the opposite
	// of what an error message is for.
	st := env.ErrStyle
	return fmt.Errorf("%w\n\n"+
		"Start one:  %s               %s\n"+
		"            %s   %s\n\n"+
		"%s", err,
		st.Cmd("je up"), st.Muted("(control plane, worker and web client, for real)"),
		st.Cmd("je up --foreground"), st.Muted("(both in this terminal, registering nothing)"),
		st.Muted("Nothing is scheduled while it is down."))
}
