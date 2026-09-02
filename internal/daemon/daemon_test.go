package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/paths"
)

// startDaemon runs a daemon on an ephemeral port and returns its base URL.
// It blocks until the daemon is listening, so tests never poll.
func startDaemon(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	layout := paths.Layout{Data: dir, Jobs: dir + "/jobs"}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- daemon.Run(ctx, daemon.Config{
			Layout: layout,
			// Port 0: the OS picks a free one, so tests never collide with a
			// real daemon or with each other. The runtime file is what makes
			// this discoverable, which is the same mechanism the CLI uses.
			Addr:    "127.0.0.1:0",
			Version: "test",
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			Ready:   ready,
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not become ready")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon.Run returned: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("daemon did not shut down")
		}
	})

	info, err := daemon.ReadRuntime(layout.Runtime())
	if err != nil {
		t.Fatalf("reading runtime file: %v", err)
	}
	return "http://" + info.Address
}

func TestHealthEndpoint(t *testing.T) {
	base := startDaemon(t)

	var health struct {
		Version string `json:"version"`
		DataDir string `json:"data_dir"`
	}
	getJSON(t, base+"/v1/health", &health)

	if health.Version != "test" {
		t.Errorf("version = %q, want %q", health.Version, "test")
	}
	if health.DataDir == "" {
		t.Error("health did not report a data dir")
	}
}

func TestEmitThenListRoundTrip(t *testing.T) {
	base := startDaemon(t)

	body := `{"type":"homekit.motion","payload":{"room":"office"}}`
	resp, err := http.Post(base+"/v1/events", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST /v1/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/events: status %s", resp.Status)
	}

	var events struct {
		Events []struct {
			Type    string          `json:"type"`
			Source  string          `json:"source"`
			Payload json.RawMessage `json:"payload"`
		} `json:"events"`
	}
	getJSON(t, base+"/v1/events?limit=10", &events)

	// engine.started is written on boot, so the emitted event is not alone.
	// Newest first, so ours is index 0.
	if len(events.Events) < 2 {
		t.Fatalf("got %d events, want at least 2", len(events.Events))
	}
	got := events.Events[0]
	if got.Type != "homekit.motion" {
		t.Errorf("newest event type = %q, want homekit.motion", got.Type)
	}
	// The API defaults an unspecified source to "api", since the caller is
	// another program rather than the shell (D18).
	if got.Source != "api" {
		t.Errorf("source = %q, want api", got.Source)
	}
	if string(got.Payload) != `{"room":"office"}` {
		t.Errorf("payload = %s, want {\"room\":\"office\"}", got.Payload)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	base := startDaemon(t)

	// A typo'd field is a mistake, and the endpoint should say so rather than
	// silently storing an event missing the thing the caller meant to send.
	body := `{"type":"a.thing","payloud":{}}`
	resp, err := http.Post(base+"/v1/events", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %s, want 400", resp.Status)
	}
}

func TestWrongMethodIsRejected(t *testing.T) {
	base := startDaemon(t)

	resp, err := http.Post(base+"/v1/health", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	// The mux's method patterns give a 405 rather than a confusing 404.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %s, want 405", resp.Status)
	}
}

func getJSON(t *testing.T, url string, into any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
}

// TestReadEndpointsAreServed covers the seam that made the scheduler
// unobservable: until these existed, the one process that schedules held the
// data directory lock, so `je waiting` could not answer the question it exists
// to answer while anything was actually happening.
func TestReadEndpointsAreServed(t *testing.T) {
	base := startDaemon(t)

	var jobs struct {
		Jobs []struct {
			Slug string `json:"slug"`
		} `json:"jobs"`
	}
	getJSON(t, base+"/v1/jobs", &jobs)
	// No jobs directory in the fixture, so an empty list -- and it must be a
	// list rather than null, or every client has to special-case "no rows".
	if jobs.Jobs == nil {
		t.Error("/v1/jobs returned null rather than an empty array")
	}

	var waiting struct {
		Scheduled []any `json:"scheduled"`
		Queued    []any `json:"queued"`
		Blocked   []any `json:"blocked"`
		Running   []any `json:"running"`
	}
	getJSON(t, base+"/v1/waiting", &waiting)

	var runs struct {
		Runs []any `json:"runs"`
	}
	getJSON(t, base+"/v1/runs?limit=5", &runs)
	if runs.Runs == nil {
		t.Error("/v1/runs returned null rather than an empty array")
	}
}

func TestMissingResourcesAre404(t *testing.T) {
	base := startDaemon(t)

	for _, path := range []string{"/v1/jobs/nope", "/v1/runs/9999", "/v1/runs/9999/logs"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %s, want 404", path, resp.Status)
		}
	}

	// A non-numeric run id is the caller's mistake, not a missing resource.
	resp, err := http.Get(base + "/v1/runs/abc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /v1/runs/abc = %s, want 400", resp.Status)
	}
}
