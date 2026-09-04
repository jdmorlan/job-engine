package daemon_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/paths"
	"github.com/jdmorlan/job-engine/internal/store"
	"github.com/jdmorlan/job-engine/internal/worker"
)

// client talks to the daemon the current test started. Set by startDaemonIn,
// because it has to verify against an authority that did not exist until then.
// Tests in a package run sequentially, so one is enough.
//
// This file used to run every test twice, once over plaintext and once over
// TLS, so that anything quietly depending on the plaintext listener would show
// up before it was removed. It has been removed (D25), so there is one pass.
var client = http.DefaultClient

// startDaemon runs a daemon on an ephemeral port and returns its base URL.
// It blocks until the daemon is listening, so tests never poll.
func startDaemon(t *testing.T, jobs ...string) string {
	base, _ := startDaemonIn(t, jobs...)
	return base
}

func startDaemonIn(t *testing.T, jobs ...string) (string, paths.Layout) {
	t.Helper()

	dir := t.TempDir()
	layout := paths.Layout{Data: dir, Jobs: dir + "/jobs"}

	// Optional job fixtures, given as alternating name and shell script.
	if len(jobs) > 0 {
		if err := os.MkdirAll(layout.Jobs, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(jobs); i += 2 {
			body := fmt.Sprintf("command: [\"/bin/sh\", \"-c\", %q]\n", jobs[i+1])
			if err := os.WriteFile(
				filepath.Join(layout.Jobs, jobs[i]+".yaml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

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
	if !info.TLS {
		t.Fatal("the runtime file does not say this control plane serves TLS")
	}
	client = httpClient(t, layout)
	return "https://" + info.Address, layout
}

// httpClient verifies the control plane against its own authority, which is the
// only way to talk to one (D25).
func httpClient(t *testing.T, layout paths.Layout) *http.Client {
	t.Helper()
	body, err := os.ReadFile(layout.CACert())
	if err != nil {
		t.Fatalf("reading the control plane's authority: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		t.Fatal("the control plane's authority is not a certificate")
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
}

// startDaemonWithWorker is startDaemon plus a real worker, over real HTTP.
//
// This is the end-to-end shape D20 describes and the only test that exercises
// it whole: the control plane queues, the worker claims over the API, runs the
// process, ships logs back, and reports a completion. Everything the engine
// tests fake with an in-process helper is genuine here, including the transport
// and the JSON encoding of a Dispatch.
func startDaemonWithWorker(t *testing.T, jobs ...string) string {
	t.Helper()

	base, layout := startDaemonIn(t, jobs...)
	addr := strings.TrimPrefix(base, "https://")

	// The worker enrols itself the way a local one does, from the token the
	// control plane published (D25), and then presents the certificate it was
	// issued -- this test is the only one that exercises the whole shape D20
	// describes, transport included.
	wc, err := dialWorker(t, addr, layout)
	if err != nil {
		t.Fatal(err)
	}

	// Asked over the API rather than passed in, which is what a worker on
	// another machine would have to do anyway.
	var health struct {
		JobsDir string `json:"jobs_dir"`
	}
	getJSON(t, base+"/v1/health", &health)

	w, err := worker.New(worker.Options{
		Name:        "test-worker",
		Labels:      []string{store.DefaultLabel},
		Concurrency: 2,
		Version:     "test",
		JobsDir:     health.JobsDir,
		Client:      wc,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("worker.Run: %v", err)
		}
	}()

	// The worker is stopped before the daemon (t.Cleanup is LIFO and this
	// registers later), so it is never left claiming against a closed store.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("worker did not shut down")
		}
	})
	return base
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
	resp, err := client.Post(base+"/v1/events", "application/json", bytes.NewBufferString(body))
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
	resp, err := client.Post(base+"/v1/events", "application/json", bytes.NewBufferString(body))
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

	resp, err := client.Post(base+"/v1/health", "application/json", nil)
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
	resp, err := client.Get(url)
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
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %s, want 404", path, resp.Status)
		}
	}

	// A non-numeric run id is the caller's mistake, not a missing resource.
	resp, err := client.Get(base + "/v1/runs/abc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /v1/runs/abc = %s, want 400", resp.Status)
	}
}

// triggerRun posts a run and returns its id.
func triggerRun(t *testing.T, base, job string) int64 {
	t.Helper()
	resp, err := client.Post(base+"/v1/runs", "application/json",
		strings.NewReader(`{"job":"`+job+`"}`))
	if err != nil {
		t.Fatalf("POST /v1/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /v1/runs: %s: %s", resp.Status, body)
	}
	var run struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	// Accepted, not completed: the request queues work and returns, so a job
	// may run for its full hour without tying up a connection.
	if run.Status != "queued" {
		t.Errorf("status = %q, want queued", run.Status)
	}
	return run.ID
}

// collectStream follows a run to completion, returning its log lines and final
// status.
func collectStream(t *testing.T, base string, runID int64) ([]string, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/runs/%d/stream", base, runID), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	var (
		lines  []string
		status string
	)
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Kind   string `json:"kind"`
			Line   string `json:"line"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("parsing stream event: %v", err)
		}
		switch ev.Kind {
		case "log":
			lines = append(lines, ev.Line)
		case "done":
			return lines, ev.Status
		}
	}
	if status == "" {
		t.Fatal("stream ended without a done event")
	}
	return lines, status
}

func TestRunTriggeredAndStreamedOverHTTP(t *testing.T) {
	base := startDaemonWithWorker(t, "talker", `echo one; echo two; echo three`)

	id := triggerRun(t, base, "talker")
	lines, status := collectStream(t, base, id)

	if status != "succeeded" {
		t.Errorf("status = %q", status)
	}
	want := []string{"one", "two", "three"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d: %q", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestStreamAttachedLateStillSeesEverything is the race the endpoint exists to
// avoid. The POST returns before the client can open the stream, so a fast job
// may already have produced -- or finished producing -- its output. Replaying
// stored lines before following live ones is what makes that safe, and getting
// it backwards would lose exactly the first lines of a fast job.
func TestStreamAttachedLateStillSeesEverything(t *testing.T) {
	base := startDaemonWithWorker(t, "quick", `echo alpha; echo beta`)

	id := triggerRun(t, base, "quick")

	// Deliberately late: by now the run has almost certainly finished.
	time.Sleep(1500 * time.Millisecond)

	lines, status := collectStream(t, base, id)
	if status != "succeeded" {
		t.Errorf("status = %q", status)
	}
	if len(lines) != 2 || lines[0] != "alpha" || lines[1] != "beta" {
		t.Errorf("a late subscriber saw %q, want both lines replayed from storage", lines)
	}
}

func TestStreamDoesNotDuplicateReplayedLines(t *testing.T) {
	base := startDaemonWithWorker(t, "slowish", `echo first; sleep 1; echo second`)

	id := triggerRun(t, base, "slowish")
	// Attach mid-run: "first" is already stored, "second" is still to come.
	time.Sleep(600 * time.Millisecond)

	lines, _ := collectStream(t, base, id)
	seen := map[string]int{}
	for _, l := range lines {
		seen[l]++
	}
	for line, n := range seen {
		if n > 1 {
			t.Errorf("line %q was delivered %d times; replay and live overlapped", line, n)
		}
	}
	if seen["first"] != 1 || seen["second"] != 1 {
		t.Errorf("lines = %q, want first and second exactly once each", lines)
	}
}

func TestOverlapSkipIsAConflict(t *testing.T) {
	base := startDaemon(t, "hog", `sleep 5`)

	triggerRun(t, base, "hog")
	time.Sleep(500 * time.Millisecond)

	resp, err := client.Post(base+"/v1/runs", "application/json",
		strings.NewReader(`{"job":"hog"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 409 rather than 400: the request was well formed and the engine declined
	// for a reason the caller can understand and act on.
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second trigger = %s, want 409", resp.Status)
	}
}

func TestTriggerUnknownJobIs404(t *testing.T) {
	base := startDaemon(t)

	resp, err := client.Post(base+"/v1/runs", "application/json",
		strings.NewReader(`{"job":"nope"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %s, want 404", resp.Status)
	}
}

// dialWorker enrols a worker against the running control plane and returns a
// client presenting the certificate it was issued.
func dialWorker(t *testing.T, addr string, layout paths.Layout) (*worker.Client, error) {
	t.Helper()

	token, err := os.ReadFile(layout.BootstrapToken())
	if err != nil {
		t.Fatalf("the control plane published no local enrolment token: %v", err)
	}
	caPEM, err := os.ReadFile(layout.BootstrapCA())
	if err != nil {
		t.Fatalf("the control plane published no authority: %v", err)
	}

	key, pubPEM := workerKeypair(t)
	body, _ := json.Marshal(map[string]any{
		"token": strings.TrimSpace(string(token)), "public_key": pubPEM,
		"name": "test-worker", "labels": []string{store.DefaultLabel},
	})
	resp, err := client.Post("https://"+addr+"/v1/enrol", "application/json",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("enrolling: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Certificate string `json:"certificate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Certificate == "" {
		t.Fatalf("enrolment returned no certificate (status %s)", resp.Status)
	}

	writeFile(t, layout.IdentityKey(), key)
	writeFile(t, layout.IdentityCert(), out.Certificate)
	writeFile(t, layout.IdentityCA(), string(caPEM))

	return worker.DialTLS(addr, layout.IdentityCert(), layout.IdentityKey(),
		layout.IdentityCA())
}

func workerKeypair(t *testing.T) (keyPEM, pubPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
