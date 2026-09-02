package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/paths"
)

// ErrNoDaemon means we could not find or reach a running engine.
//
// It is a distinct error because the remedy is distinct: nearly every other
// failure wants you to look at a job, and this one wants you to start the
// daemon. Commands wrap it with that advice rather than printing a connection
// refused.
var ErrNoDaemon = errors.New("no engine is running")

// Client is a thin HTTP client for the daemon API.
//
// D19's R2 says every command you learned locally must work against a remote
// engine by switching context. That is why the address is a field resolved at
// connect time rather than a constant: `--context cluster` will set it from
// config, and nothing else in the CLI will need to change.
type Client struct {
	base *url.URL
	http *http.Client
}

// Connect locates the daemon for a data directory.
//
// Resolution order: the JE_ADDR environment variable, then the runtime file
// the daemon publishes on start. There is no default fallback: guessing the
// default port and then failing to connect produces a worse error message than
// noticing there is no runtime file at all.
func Connect(l paths.Layout) (*Client, error) {
	addr := os.Getenv("JE_ADDR")
	if addr == "" {
		info, err := daemon.ReadRuntime(l.Runtime())
		switch {
		case os.IsNotExist(err):
			return nil, fmt.Errorf("%w for %s", ErrNoDaemon, l.Data)
		case err != nil:
			return nil, err
		}
		addr = info.Address
	}

	base, err := url.Parse("http://" + addr)
	if err != nil {
		return nil, fmt.Errorf("bad engine address %q: %w", addr, err)
	}
	return &Client{
		base: base,
		// No overall timeout: `je logs -f` will stream indefinitely. Per-request
		// deadlines belong on the context the caller passes.
		http: &http.Client{},
	}, nil
}

func (c *Client) Health(ctx context.Context) (engine.Health, error) {
	return do[engine.Health](ctx, c, http.MethodGet, "/v1/health", nil)
}

func (c *Client) Emit(ctx context.Context, req api.EmitRequest) (api.EmitResponse, error) {
	return do[api.EmitResponse](ctx, c, http.MethodPost, "/v1/events", req)
}

type eventList struct {
	Events []model.Event `json:"events"`
}

func (c *Client) Events(ctx context.Context, limit int) ([]model.Event, error) {
	path := fmt.Sprintf("/v1/events?limit=%d", limit)
	list, err := do[eventList](ctx, c, http.MethodGet, path, nil)
	return list.Events, err
}

// do issues one request and decodes the response into T.
//
// Generic over the response type so each endpoint method is one line and there
// is exactly one place that knows how the wire protocol reports errors.
func do[T any](ctx context.Context, c *Client, method, path string, body any) (T, error) {
	var zero T

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, reader)
	if err != nil {
		return zero, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A refused connection here almost always means the daemon died
		// without cleaning up its runtime file. Say the useful thing.
		return zero, fmt.Errorf("%w at %s: %w", ErrNoDaemon, c.base.Host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return zero, decodeError(resp)
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, fmt.Errorf("decoding %s response: %w", path, err)
	}
	return out, nil
}

func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return errors.New(parsed.Error.Message)
	}
	return fmt.Errorf("engine returned %s: %s", resp.Status, bytes.TrimSpace(body))
}

// requestTimeout bounds an ordinary request/response command. Streaming
// commands do not use it.
const requestTimeout = 10 * time.Second

// withTimeout wraps ctx for a single non-streaming request.
func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, requestTimeout)
}
