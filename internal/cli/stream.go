package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

// TriggerRun asks the daemon to queue a run and returns it immediately.
func (c *Client) TriggerRun(ctx context.Context, job, actor string) (store.Run, error) {
	return do[store.Run](ctx, c, http.MethodPost, "/v1/runs",
		api.TriggerRequest{Job: job, Actor: actor})
}

// RetryRun asks the control plane to add an attempt to an existing run.
func (c *Client) RetryRun(ctx context.Context, id int64, actor string) (store.Run, error) {
	return do[store.Run](ctx, c, http.MethodPost,
		"/v1/runs/"+strconv.FormatInt(id, 10)+"/retry", api.RetryRequest{Actor: actor})
}

// RunDetail fetches the full picture of a run.
func (c *Client) RunDetail(ctx context.Context, id int64) (engine.RunDetail, error) {
	return do[engine.RunDetail](ctx, c, http.MethodGet,
		"/v1/runs/"+strconv.FormatInt(id, 10)+"/detail", nil)
}

// StreamRun follows a run's output, calling onEvent for each event, and
// returns when the run finishes or the context is cancelled.
//
// Deliberately not built on `do`: that function decodes one JSON body, and
// this consumes an open stream. Sharing them would mean bending the common
// path around the exceptional one.
func (c *Client) StreamRun(ctx context.Context, id int64, onEvent func(engine.StreamEvent)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base.String()+"/v1/runs/"+strconv.FormatInt(id, 10)+"/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrNoControlPlane, c.base.Host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeError(resp)
	}

	// A log line can be long, so the scanner gets a generous cap. The engine
	// truncates at 1MB on capture, which bounds what can arrive here.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 2<<20)

	for sc.Scan() {
		line := sc.Text()
		// Only the data field matters; the event name duplicates ev.Kind, and
		// comments (": keep-alive") are skipped by both checks below.
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev engine.StreamEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		onEvent(ev)
		if ev.Kind == engine.StreamDone {
			return nil
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("reading run stream: %w", err)
	}
	return nil
}
