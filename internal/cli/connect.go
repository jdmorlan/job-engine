package cli

import (
	"context"
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
// It bought one saved command (`je quickstart`) and cost a second implementation of
// every read. See v0.6's rule: a capability is not a reason.
func withClient(ctx context.Context, env *Env, fn func(context.Context, *Client) error) error {
	client, err := Connect(env.Layout)
	if err != nil {
		return adviseNoControlPlane(err)
	}
	if !reachable(ctx, client) {
		return adviseNoControlPlane(fmt.Errorf("%w at %s", ErrNoControlPlane, client.base.Host))
	}
	return fn(ctx, client)
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
func adviseNoControlPlane(err error) error {
	// `je control-plane run` is deliberately not the headline. It starts a
	// control plane with no worker, which runs nothing (C11) -- suggesting it
	// first sends somebody to a second, quieter failure, which is the opposite
	// of what an error message is for.
	return fmt.Errorf("%w\n\n"+
		"Start one:  je quickstart          (control plane and a worker, this terminal)\n"+
		"            docker compose up -d   (unattended)\n\n"+
		"Nothing is scheduled while it is down.", err)
}
