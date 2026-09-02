package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/model"
)

func init() {
	register(&Command{
		Name:  "emit",
		Args:  "<type> [flags]",
		Usage: "emit an event into the engine",
		Long: "This is the engine's only ingress (D16). The engine knows nothing about\n" +
			"the outside world; anything that can run a command can be an event source:\n" +
			"a HomeKit Shortcut, a git hook, a CI pipeline, a phone, another app.\n\n" +
			"  je emit homekit.motion --payload '{\"room\":\"office\"}'",
		Run: runEmit,
	})
}

func runEmit(ctx context.Context, env *Env, args []string) error {
	cmd := commands["emit"]
	fs := newFlagSet(cmd, env)
	payload := fs.String("payload", "", "JSON object to attach to the event")
	dedupeKey := fs.String("dedupe-key", "", "emit at most one event for this key, ever")
	actor := fs.String("actor", "", "who is responsible, if a person is")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("expected exactly one event type, got %d", len(positional))
	}

	req := api.EmitRequest{
		Type:   positional[0],
		Source: model.SourceCLI,
		Actor:  *actor,
	}
	if *payload != "" {
		// Validate here rather than at the server, so a malformed payload is
		// caught before it becomes a stored event nobody can interpret.
		if !json.Valid([]byte(*payload)) {
			return usagef("--payload is not valid JSON")
		}
		req.Payload = json.RawMessage(*payload)
	}
	if *dedupeKey != "" {
		req.DedupeKey = dedupeKey
	}

	client, err := Connect(env.Layout)
	if err != nil {
		return adviseNoDaemon(err)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	resp, err := client.Emit(ctx, req)
	if err != nil {
		return adviseNoDaemon(err)
	}

	// Report the dedupe outcome explicitly. Silently succeeding on a duplicate
	// would make "I emitted it but nothing happened" unexplainable, which is
	// precisely the confusion P1 exists to prevent.
	if resp.Deduped {
		fmt.Fprintf(env.Stdout, "already emitted: event %d (%s) at %s\n",
			resp.Event.ID, resp.Event.Type, resp.Event.CreatedAt.Local().Format("15:04:05"))
		return nil
	}
	fmt.Fprintf(env.Stdout, "event %d %s\n", resp.Event.ID, resp.Event.Type)
	return nil
}
