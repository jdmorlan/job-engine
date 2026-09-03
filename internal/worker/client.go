package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/store"
)

// Client is a worker's connection to the control plane.
//
// Everything the worker knows arrives through it, and everything it learns goes
// back the same way. There is no second channel and no direct database access
// (C1), which is what makes the control plane's timeline complete rather than a
// partial view assembled from two places.
type Client struct {
	base *url.URL
	http *http.Client
}

// Dial returns a client for an address like "127.0.0.1:7620".
func Dial(addr string) (*Client, error) {
	base, err := url.Parse("http://" + addr)
	if err != nil {
		return nil, fmt.Errorf("bad control plane address %q: %w", addr, err)
	}
	return &Client{base: base, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// DialTLS connects presenting this machine's issued identity, verifying the
// control plane against the authority it enrolled with (D25 step 5).
//
// No system trust store is involved in either direction. The control plane is
// trusted because this worker enrolled with it, and the worker is trusted
// because that same authority signed its certificate -- which is what makes
// this closed, with no domain to own and no public CA to depend on.
func DialTLS(addr, certPath, keyPath, caPath string) (*Client, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading this worker's identity: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("reading the control plane's authority: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s is not a certificate", caPath)
	}
	base, err := url.Parse("https://" + addr)
	if err != nil {
		return nil, fmt.Errorf("bad control plane address %q: %w", addr, err)
	}
	return &Client{
		base: base,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
				MinVersion:   tls.VersionTLS12,
			}},
		},
	}, nil
}

// Addr reports where this client is pointed, for log lines and errors.
func (c *Client) Addr() string { return c.base.Host }

func (c *Client) Register(ctx context.Context, w store.Worker) (store.Worker, error) {
	return do[store.Worker](ctx, c, http.MethodPost, "/v1/workers", w)
}

func (c *Client) Heartbeat(ctx context.Context, id string, holding []int64) ([]int64, error) {
	out, err := do[api.HeartbeatResponse](ctx, c, http.MethodPost,
		"/v1/workers/"+url.PathEscape(id)+"/heartbeat",
		api.HeartbeatRequest{Holding: holding})
	return out.Revoked, err
}

// Claim asks for work. A nil Dispatch with a nil error means there is none,
// which is the ordinary case.
func (c *Client) Claim(ctx context.Context, id string) (*engine.Dispatch, error) {
	out, err := do[api.ClaimResponse](ctx, c, http.MethodPost,
		"/v1/workers/"+url.PathEscape(id)+"/claim", nil)
	if err != nil {
		return nil, err
	}
	return out.Dispatch, nil
}

func (c *Client) AppendLogs(ctx context.Context, runID int64, attempt int, lines []engine.LogLine) error {
	_, err := do[struct{}](ctx, c, http.MethodPost,
		fmt.Sprintf("/v1/runs/%d/logs", runID),
		api.AppendLogsRequest{Attempt: attempt, Lines: lines})
	return err
}

func (c *Client) Complete(ctx context.Context, runID int64, workerID string, comp engine.Completion) error {
	_, err := do[struct{}](ctx, c, http.MethodPost,
		fmt.Sprintf("/v1/runs/%d/complete", runID),
		api.CompleteRequest{WorkerID: workerID, Completion: comp})
	return err
}

// do issues one request and decodes the response into T.
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
		return zero, fmt.Errorf("control plane at %s: %w", c.base.Host, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return zero, nil
	}
	if resp.StatusCode >= 400 {
		return zero, decodeError(resp)
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil && err != io.EOF {
		return zero, fmt.Errorf("decoding %s response: %w", path, err)
	}
	return out, nil
}

// StatusError is a refusal from the control plane that carries its status, so
// a caller can tell "you are wrong" from "the server broke" without matching on
// the wording of a sentence.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		return &StatusError{Status: resp.StatusCode, Message: parsed.Error.Message}
	}
	return &StatusError{
		Status:  resp.StatusCode,
		Message: fmt.Sprintf("control plane returned %s: %s", resp.Status, bytes.TrimSpace(body)),
	}
}

// SourceTree downloads one pinned source tree. The caller closes the body.
//
// Not routed through do[T]: this is an archive, not JSON, and buffering a
// repository into memory to hand back a []byte would be a size limit waiting to
// be discovered.
func (c *Client) SourceTree(ctx context.Context, name, revision string) (io.ReadCloser, error) {
	url := c.base.String() + "/v1/sources/" + url.PathEscape(name) + "/tree/" + url.PathEscape(revision)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp.Body, nil
}
