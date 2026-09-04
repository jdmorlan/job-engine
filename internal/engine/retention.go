package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/store"
)

// EventRetentionSwept records one pass of the sweep (D13).
//
// The counts are on the event because deletion is the one operation that
// erases its own evidence: after a sweep, thirty days of history for a job that
// ran daily for a year is indistinguishable from a job that started thirty days
// ago. What was removed has to be written down as it goes, or the question
// "was there more?" has no answer anywhere (P1).
const EventRetentionSwept = "retention.swept"

// Sweep is what `je retention sweep` asks the control plane to do.
//
// It runs here rather than in the CLI for the reason C1 gives: the worker
// executing the job has no access to the store, and should not. The job is a
// client making one request, exactly as a person would.
type Sweep struct {
	// Space is what the logs database gave back to the disk.
	Space store.LogSpace `json:"space"`
}

// Sweep removes history past its keep period and returns the space (D13).
//
// Deleting rows is not yet here; this pass reclaims what previous deletions
// already freed, which on a database that predates D13 also means converting it
// so that reclaiming is possible at all. That ordering is deliberate: the space
// half is the half that silently does nothing when it is wrong, so it is worth
// having working and visible before anything starts removing rows.
func (e *Engine) Sweep(ctx context.Context, actor string) (Sweep, error) {
	space, err := e.store.ReclaimLogSpace(ctx, 0)
	if err != nil {
		return Sweep{}, err
	}
	out := Sweep{Space: space}

	payload, _ := json.Marshal(map[string]any{
		"converted":    space.Converted,
		"pages_freed":  space.PagesFreed,
		"bytes_freed":  space.Reclaimed(),
		"logs_db_size": space.BytesAfter,
	})
	if _, _, err := e.publish(ctx, model.Event{
		Type:      EventRetentionSwept,
		Source:    model.SourceEngine,
		Payload:   payload,
		Actor:     actor,
		CreatedAt: e.now(),
	}); err != nil {
		return out, fmt.Errorf("recording %s: %w", EventRetentionSwept, err)
	}
	e.log.Info("retention swept",
		"pages_freed", space.PagesFreed, "bytes_freed", space.Reclaimed(),
		"converted", space.Converted)
	return out, nil
}
