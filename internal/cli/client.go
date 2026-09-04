package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/ca"
	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/jobdef"
	"github.com/jdmorlan/job-engine/internal/model"
	"github.com/jdmorlan/job-engine/internal/paths"
	"github.com/jdmorlan/job-engine/internal/store"
)

// ErrNoControlPlane means we could not find or reach a running control plane.
//
// It is a distinct error because the remedy is distinct: nearly every other
// failure wants you to look at a job, and this one wants you to start the
// control plane. Commands wrap it with that advice rather than printing a
// connection refused.
//
// Named for the component (F1, v0.6) rather than for "daemon". That word is a
// deployment form, not a component: the thing you cannot reach is the same
// whether it is a container, a foreground `je control-plane run`, or a service.
var ErrNoControlPlane = errors.New("no control plane is running")

// Client is the CLI's only way to reach the system (D20/C11).
//
// D19's R2 says every command you learned locally must work against a remote
// engine by switching context. That is why the address is a field resolved at
// connect time rather than a constant: `--context cluster` will set it from
// config, and nothing else in the CLI will need to change.
type Client struct {
	base *url.URL
	http *http.Client
}

// Connect locates the control plane for a data directory.
func Connect(l paths.Layout) (*Client, error) {
	addr, err := resolveAddr(l)
	if err != nil {
		return nil, err
	}

	// There is one transport now (D25), so there is nothing to detect -- but a
	// control plane from before the flip is still a thing somebody can be
	// pointed at, and it deserves a sentence rather than a handshake error.
	if err := refusePlaintext(l); err != nil {
		return nil, err
	}
	pool, err := authorityPool(l)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	// Present this machine's identity when it has one. The CLI used to present
	// nothing on the grounds that reads need no identity, which is still true --
	// but writes do now, and `je run` is a write (D25).
	//
	// The same files a worker uses, because a machine has one identity whatever
	// roles it carries: a laptop enrolled as both a client and a `macos` worker
	// is one certificate, and issuing it two would make "who did this" depend on
	// which command was running.
	if cert, err := identityKeyPair(l); err == nil {
		tlsConfig.Certificates = []tls.Certificate{cert}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}

	base, err := url.Parse("https://" + dialable(addr))
	if err != nil {
		return nil, fmt.Errorf("bad engine address %q: %w", addr, err)
	}
	return &Client{
		base: base,
		// No overall timeout: `je logs -f` will stream indefinitely. Per-request
		// deadlines belong on the context the caller passes.
		http: &http.Client{Transport: transport},
	}, nil
}

// dialable turns a recorded bind address into one a client can connect to and
// verify.
//
// A control plane that bound 0.0.0.0 records exactly that, and it is a bind
// address rather than a destination: nothing certifies it, so a TLS client
// checking the hostname rejects a certificate that is otherwise perfectly
// correct. Whoever wrote 0.0.0.0 meant "every interface", and a client on the
// same machine reaching "every interface" means loopback.
func dialable(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "0.0.0.0", "::", "":
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// refusePlaintext names the one incompatibility the flip introduces.
//
// A runtime file with no `tls` in it was written by a control plane from before
// D25 removed the plaintext listener. Everything on this side now speaks HTTPS,
// so talking to it would fail during the handshake and blame the wrong thing --
// Go would report a malformed record, which is true and tells nobody which of
// the two processes is out of date.
//
// Only when there IS a runtime file. An address from JE_ADDR or the endpoint
// file says nothing about the process behind it, and guessing "old" from
// silence would refuse to connect to a perfectly current control plane.
func refusePlaintext(l paths.Layout) error {
	info, err := daemon.ReadRuntime(l.Runtime())
	if err != nil || info.TLS {
		return nil
	}
	return fmt.Errorf(
		"the control plane at %s speaks plaintext, and this je only speaks TLS.\n"+
			"It is running a version from before certificates became mandatory.\n"+
			"Upgrade it and restart it -- it will issue its own authority on start,\n"+
			"and any worker attached to it has to be restarted too.",
		info.Address)
}

// authorityPath is the control plane's certificate, wherever this machine has a
// copy of it.
//
// Three places, most authoritative first: the control plane's own directory,
// the copy a worker kept when it enrolled, and the one published in the
// bootstrap directory for a worker on this machine. A CLI beside a control
// plane has the first, one beside a remote worker has the second, and one in a
// container that mounts only the bootstrap volume has the third.
func authorityPath(l paths.Layout) (string, error) {
	candidates := []string{l.CACert(), l.IdentityCA(), l.BootstrapCA()}
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// Nothing on disk, but this machine may be the one running the control
	// plane in a container it installed. The authority is then a `docker cp`
	// away, and doing that here is the difference between a CLI that works and
	// one that tells somebody to go and use docker (D20/D25).
	if err := adoptContainerAuthority(l); err == nil {
		return l.IdentityCA(), nil
	}

	return "", fmt.Errorf(
		"no control plane authority on this machine.\n"+
			"Looked in:\n  %s\n\n"+
			"Every connection is verified against the authority the control plane\n"+
			"issues from (D25), so a machine that has never met one has nothing to\n"+
			"check against.\n\n"+
			"A worker gets one by enrolling:\n"+
			"  je enroll <name>                                            (there)\n"+
			"  je worker run --token <t> --ca-pin <fp> --addr <host:port>  (here)\n\n"+
			"Anything else needs a copy of the control plane's ca.crt at the second\n"+
			"path above.",
		strings.Join(candidates, "\n  "))
}

// adoptContainerAuthority takes the CA out of a control plane container on this
// machine, once, so that every later command finds it on disk like any other.
//
// Silent when there is no such container: that is the ordinary case for a CLI
// on somebody's laptop, and it falls through to the error that explains
// enrolling.
func adoptContainerAuthority(l paths.Layout) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	name := controlPlaneContainer(l)
	if dockerAvailable() != nil || !containerNamed(ctx, name) {
		return errors.New("no control plane container on this machine")
	}
	if err := os.MkdirAll(l.Data, 0o700); err != nil {
		return err
	}
	return copyAuthorityFrom(ctx, name, l.IdentityCA())
}

// controlPlaneContainer is what this data directory's control plane container
// is called: what `install` recorded, or the name it would have generated.
func controlPlaneContainer(l paths.Layout) string {
	if e, err := ReadEndpoint(l.Endpoint()); err == nil && e.Container != "" {
		return e.Container
	}
	return containerName("control-plane")
}

// authorityPool verifies the control plane against the CA it issues from.
//
// The CLI presents no certificate of its own: it is not a worker, it has no
// identity to prove, and read endpoints need none. What it does need is to know
// it is talking to the right control plane, which this gives it without any
// public CA or system trust store being involved.
func authorityPool(l paths.Layout) (*x509.CertPool, error) {
	path, err := authorityPath(l)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the control plane's authority: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, fmt.Errorf("%s is not a certificate", path)
	}
	return pool, nil
}

// identityKeyPair loads this machine's issued certificate, if it has one.
//
// A missing file is not an error the caller has to distinguish by string: it
// returns something os.IsNotExist recognises, because "this machine has no
// identity" is an ordinary state that stays legal for reading.
func identityKeyPair(l paths.Layout) (tls.Certificate, error) {
	certPath, keyPath := l.IdentityCert(), l.IdentityKey()
	for _, path := range []string{certPath, keyPath} {
		if _, err := os.Stat(path); err != nil {
			return tls.Certificate{}, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("reading this machine's identity: %w", err)
	}
	return cert, nil
}

// DialVerified connects to a control plane verified against a CA the caller
// already has and trusts. Used during enrollment, once the authority has been
// pinned by fingerprint.
func DialVerified(addr string, caPEM []byte) (*Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("the control plane's authority is not a certificate")
	}
	base, err := url.Parse("https://" + addr)
	if err != nil {
		return nil, fmt.Errorf("bad control plane address %q: %w", addr, err)
	}
	return &Client{base: base, http: &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: pool, MinVersion: tls.VersionTLS12,
		}},
	}}, nil
}

// resolveAddr finds the control plane, in the order that puts the most
// authoritative answer first.
//
//  1. JE_ADDR, because an explicit override should always win.
//  2. The runtime file, which a live local control plane wrote about itself
//     after binding -- so it is right even when the port was 0.
//  3. The endpoint file, which `je control-plane install` wrote when it set one
//     up somewhere the runtime file cannot reach us from: a container writes
//     its runtime file inside its own volume, and the host never sees it.
//
// There is still no guessing at the default port. A wrong guess produces a
// connection error that says nothing about the real problem, which is worse
// than saying plainly that we do not know where it is.
func resolveAddr(l paths.Layout) (string, error) {
	if addr := os.Getenv("JE_ADDR"); addr != "" {
		return addr, nil
	}

	info, err := daemon.ReadRuntime(l.Runtime())
	switch {
	case err == nil:
		return info.Address, nil
	case !os.IsNotExist(err):
		return "", err
	}

	endpoint, err := ReadEndpoint(l.Endpoint())
	switch {
	case err == nil && endpoint.Address != "":
		return endpoint.Address, nil
	case err != nil && !os.IsNotExist(err):
		return "", err
	}

	return "", fmt.Errorf("%w for %s", ErrNoControlPlane, l.Data)
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
		// without cleaning up its runtime file. Say the useful thing -- and if
		// it was a certificate validity failure, say the other useful thing,
		// which is that the clocks probably disagree (D25).
		return zero, fmt.Errorf("%w at %s: %w",
			ErrNoControlPlane, c.base.Host, ca.ExplainHandshake(err))
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

// The read side of the client. Each method is one line over `do`, which is the
// payoff for making that function generic -- there is exactly one place that
// knows how the wire protocol reports errors, and adding an endpoint cannot
// get it wrong.

func (c *Client) Source() string { return "control plane at " + c.base.Host }

// Addr is where this client is pointed, for instructions a person will retype.
func (c *Client) Addr() string { return c.base.Host }

func (c *Client) Jobs(ctx context.Context) ([]store.Job, error) {
	out, err := do[struct {
		Jobs []store.Job `json:"jobs"`
	}](ctx, c, http.MethodGet, "/v1/jobs", nil)
	return out.Jobs, err
}

func (c *Client) Job(ctx context.Context, slug string) (store.Job, error) {
	return do[store.Job](ctx, c, http.MethodGet, "/v1/jobs/"+url.PathEscape(slug), nil)
}

// Definition parses the snapshot the job endpoint returns.
//
// The definition travels as its stored JSON rather than as a second endpoint,
// because that snapshot is exactly what D11 says a run executed under -- there
// is no other rendering of a definition that would be correct to show.
func (c *Client) Definition(ctx context.Context, slug string) (*jobdef.Definition, error) {
	job, err := c.Job(ctx, slug)
	if err != nil {
		return nil, err
	}
	return jobdef.FromSnapshot(job.Definition)
}

func (c *Client) Runs(ctx context.Context, jobSlug string, limit int) ([]store.Run, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if jobSlug != "" {
		q.Set("job", jobSlug)
	}
	out, err := do[struct {
		Runs []store.Run `json:"runs"`
	}](ctx, c, http.MethodGet, "/v1/runs?"+q.Encode(), nil)
	return out.Runs, err
}

func (c *Client) Run(ctx context.Context, id int64) (store.Run, error) {
	return do[store.Run](ctx, c, http.MethodGet,
		"/v1/runs/"+strconv.FormatInt(id, 10), nil)
}

func (c *Client) Logs(ctx context.Context, runID int64, attempt int) ([]store.LogLine, error) {
	path := "/v1/runs/" + strconv.FormatInt(runID, 10) + "/logs"
	if attempt > 0 {
		path += "?attempt=" + strconv.Itoa(attempt)
	}
	out, err := do[struct {
		Lines []store.LogLine `json:"lines"`
	}](ctx, c, http.MethodGet, path, nil)
	return out.Lines, err
}

func (c *Client) CurrentState(ctx context.Context, slug string) (*store.StateVersion, error) {
	out, err := do[struct {
		State *store.StateVersion `json:"state"`
	}](ctx, c, http.MethodGet, "/v1/jobs/"+url.PathEscape(slug)+"/state", nil)
	return out.State, err
}

func (c *Client) StateHistory(ctx context.Context, slug string, limit int) ([]store.StateVersion, error) {
	out, err := do[struct {
		Versions []store.StateVersion `json:"versions"`
	}](ctx, c, http.MethodGet,
		"/v1/jobs/"+url.PathEscape(slug)+"/state/history?limit="+strconv.Itoa(limit), nil)
	return out.Versions, err
}

type sourceList struct {
	Sources []engine.SourceStatus `json:"sources"`
}

func (c *Client) Sources(ctx context.Context) ([]engine.SourceStatus, error) {
	list, err := do[sourceList](ctx, c, http.MethodGet, "/v1/sources", nil)
	return list.Sources, err
}

func (c *Client) AddSource(ctx context.Context, req api.AddSourceRequest) (engine.LoadResult, error) {
	return do[engine.LoadResult](ctx, c, http.MethodPost, "/v1/sources", req)
}

func (c *Client) SyncSource(ctx context.Context, name string) (engine.LoadResult, error) {
	return do[engine.LoadResult](ctx, c, http.MethodPost, "/v1/sources/"+url.PathEscape(name)+"/sync", nil)
}

type removedSource struct {
	Tombstoned int64 `json:"tombstoned"`
}

func (c *Client) RemoveSource(ctx context.Context, name string) (int64, error) {
	out, err := do[removedSource](ctx, c, http.MethodDelete, "/v1/sources/"+url.PathEscape(name), nil)
	return out.Tombstoned, err
}

func (c *Client) Explain(ctx context.Context, slug string) (engine.Explanation, error) {
	return do[engine.Explanation](ctx, c, http.MethodGet, "/v1/jobs/"+url.PathEscape(slug)+"/explain", nil)
}

func (c *Client) Waiting(ctx context.Context) (engine.Waiting, error) {
	return do[engine.Waiting](ctx, c, http.MethodGet, "/v1/waiting", nil)
}

type chainList struct {
	Chains []engine.ChainView `json:"chains"`
}

func (c *Client) Chains(ctx context.Context) ([]engine.ChainView, error) {
	list, err := do[chainList](ctx, c, http.MethodGet, "/v1/chains", nil)
	return list.Chains, err
}

func (c *Client) Chain(ctx context.Context, name string) (engine.ChainView, error) {
	return do[engine.ChainView](ctx, c, http.MethodGet, "/v1/chains/"+url.PathEscape(name), nil)
}

// The secret side of the client (D10).
//
// `je secret` reaches the store only through these. It used to call
// secrets.Open on the local data directory, which meant that against a control
// plane anywhere but this machine it silently wrote to the wrong filesystem --
// a failure that produced no error and showed up later as a job that could not
// see its own token.

func (c *Client) Secrets(ctx context.Context) (engine.SecretsView, error) {
	return do[engine.SecretsView](ctx, c, http.MethodGet, "/v1/secrets", nil)
}

func (c *Client) SetSecret(ctx context.Context, name, value string) (engine.SetSecretResult, error) {
	return do[engine.SetSecretResult](ctx, c, http.MethodPut,
		"/v1/secrets/"+url.PathEscape(name), api.SetSecretRequest{Value: value})
}

func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	_, err := do[struct{}](ctx, c, http.MethodDelete, "/v1/secrets/"+url.PathEscape(name), nil)
	return err
}

// Workers lists the data plane (D20/C8).
func (c *Client) Workers(ctx context.Context) ([]engine.WorkerView, error) {
	out, err := do[struct {
		Workers []engine.WorkerView `json:"workers"`
	}](ctx, c, http.MethodGet, "/v1/workers", nil)
	return out.Workers, err
}

// Sync reloads definitions on the control plane (D2, D19).
func (c *Client) Sync(ctx context.Context) (engine.LoadResult, error) {
	return do[engine.LoadResult](ctx, c, http.MethodPost, "/v1/sync", nil)
}

// MintEnrollment asks the control plane for a one-time worker token.
func (c *Client) MintEnrollment(ctx context.Context, req api.MintEnrollmentRequest) (api.MintEnrollmentResponse, error) {
	return do[api.MintEnrollmentResponse](ctx, c, http.MethodPost, "/v1/enroll/tokens", req)
}

// Enroll redeems one. The request carries a public key and a token; nothing
// secret travels in either direction.
func (c *Client) Enroll(ctx context.Context, req api.EnrollRequest) (api.EnrollResponse, error) {
	return do[api.EnrollResponse](ctx, c, http.MethodPost, "/v1/enroll", req)
}

// RegisterAgeKey binds this machine's secret-reading key to its identity.
//
// The control plane takes the name from the certificate on the connection, so
// the response says who it decided this is -- which is the value worth printing
// back, because it is the name a recipient list will use.
func (c *Client) RegisterAgeKey(ctx context.Context, recipient string) (string, error) {
	out, err := do[api.AgeKeyResponse](ctx, c, http.MethodPost, "/v1/identity/age-key",
		api.AgeKeyRequest{Recipient: recipient})
	return out.Name, err
}

// AgeKeyFor resolves an identity's name to the key it reads with, so `je secret
// recipients add` can name a machine rather than take a pasted key.
func (c *Client) AgeKeyFor(ctx context.Context, name string) (string, error) {
	out, err := do[api.AgeKeyResponse](ctx, c, http.MethodGet,
		"/v1/identities/"+url.PathEscape(name)+"/age-key", nil)
	return out.Recipient, err
}
