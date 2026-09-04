package daemon_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jdmorlan/job-engine/internal/api"
	"github.com/jdmorlan/job-engine/internal/ca"
	"github.com/jdmorlan/job-engine/internal/daemon"
	"github.com/jdmorlan/job-engine/internal/engine"
	"github.com/jdmorlan/job-engine/internal/paths"
	"github.com/jdmorlan/job-engine/internal/store"
	"github.com/jdmorlan/job-engine/internal/testsupport"
	"github.com/jdmorlan/job-engine/internal/worker"
)

// A worker renews its own certificate over the connection the certificate
// authenticates, while it is still valid -- which is what makes a 24-hour leaf
// affordable and lets this CA skip revocation entirely (D25).
//
// Lifetimes are compressed so the renewal is observable; nothing else about the
// path is changed.
func TestAWorkerRenewsItsOwnCertificate(t *testing.T) {
	restore := compressLifetimes(t, 6*time.Second, 5*time.Second)
	defer restore()

	base, layout := startTLSDaemon(t)
	client := enrollAWorker(t, base, layout, "renewer", []string{store.DefaultLabel})

	if _, ok := client.NotAfter(); !ok {
		t.Fatal("the enrolled client has no certificate")
	}
	before := read(t, layout.IdentityCert())
	beforeKey := read(t, layout.IdentityKey())

	// Inside RenewBefore already, since the whole lifetime is shorter than it.
	if err := client.Renew(context.Background()); err != nil {
		t.Fatalf("renewing: %v", err)
	}

	// Compared by certificate rather than by expiry: two issued in the same
	// second share a NotAfter, and what matters is that this is a different
	// certificate over a different key.
	after := read(t, layout.IdentityCert())
	if after == before {
		t.Error("the certificate on disk did not change")
	}
	if read(t, layout.IdentityKey()) == beforeKey {
		t.Error("the key was reused; a leaked key should stop being useful when " +
			"its certificate expires, not live as long as the worker")
	}

	// The renewed certificate has to work on the very next request, without
	// rebuilding the client -- that is the point of swapping it in place.
	if _, err := registerWith(client); err != nil {
		t.Fatalf("the renewed certificate does not authenticate: %v", err)
	}

	// And it has to survive a restart, so the files on disk must be the new
	// ones rather than only the copy in memory.
	reloaded, err := worker.DialTLS(strings.TrimPrefix(base, "https://"),
		layout.IdentityCert(), layout.IdentityKey(), filepath.Join(layout.Data, "ca.crt"))
	if err != nil {
		t.Fatalf("reloading the renewed identity from disk: %v", err)
	}
	if _, err := registerWith(reloaded); err != nil {
		t.Fatalf("the renewed identity was not written to disk: %v", err)
	}
}

// Renewal is authenticated by the certificate being replaced. A connection with
// none is not somebody whose identity can be reissued.
func TestRenewalWithoutACertificateIsRefused(t *testing.T) {
	base, _ := startTLSDaemon(t)

	// Verified transport, no client certificate: what the CLI looks like.
	resp, err := insecureClient().Post(base+"/v1/enroll/renew", "application/json",
		strings.NewReader(`{"public_key":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a connection that proved nothing", resp.StatusCode)
	}
}

// The CLI and the web client present no certificate and must keep working:
// requiring one would make identity a thing that breaks every read command.
func TestReadEndpointsWorkWithoutACertificate(t *testing.T) {
	base, _ := startTLSDaemon(t)

	resp, err := insecureClient().Get(base + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 -- a client with no identity may still read", resp.StatusCode)
	}
}

func compressLifetimes(t *testing.T, leaf, renewBefore time.Duration) func() {
	t.Helper()
	oldLeaf, oldRenew := ca.LeafLifetime, ca.RenewBefore
	ca.LeafLifetime, ca.RenewBefore = leaf, renewBefore
	return func() { ca.LeafLifetime, ca.RenewBefore = oldLeaf, oldRenew }
}

func startTLSDaemon(t *testing.T) (base string, layout paths.Layout) {
	t.Helper()
	dir := t.TempDir()
	layout = paths.Layout{Data: dir}

	// One job, in a repository, because that is the only place a job can be.
	tree := filepath.Join(dir, "repo")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "hello.yaml"),
		[]byte("command: [\"/bin/sh\", \"-c\", \"true\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := testsupport.NewGitHub(t)
	hub.Add("you/src", tree)

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Config{
			Layout: layout, Addr: "127.0.0.1:0", Version: "test",
			GitHubAPI: hub.URL,
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Ready:     ready,
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon did not start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not start")
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})

	info, err := daemon.ReadRuntime(layout.Runtime())
	if err != nil {
		t.Fatal(err)
	}
	if !info.TLS {
		t.Fatal("the runtime file does not record that this control plane serves TLS")
	}
	base = "https://" + info.Address

	// Registered before any client identity exists, so this is an ungated
	// write -- the same order a real first-run has.
	body, _ := json.Marshal(api.AddSourceRequest{
		Name: "src", Kind: store.SourceKindGitHub, Location: "you/src",
	})
	resp, err := insecureClient().Post(base+"/v1/sources", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("registering the fixture source: %s", resp.Status)
	}
	return base, layout
}

// enrollAWorker performs the real flow: mint, redeem, write the identity, and
// return a client that presents it.
func enrollAWorker(t *testing.T, base string, layout paths.Layout, name string, labels []string) *worker.Client {
	t.Helper()

	body, _ := json.Marshal(api.MintEnrollmentRequest{Name: name, Labels: labels})
	resp, err := insecureClient().Post(base+"/v1/enroll/tokens", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var mint api.MintEnrollmentResponse
	if err := json.NewDecoder(resp.Body).Decode(&mint); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	key, pubPEM := keypairPEM(t)
	body, _ = json.Marshal(api.EnrollRequest{Token: mint.Token, PublicKey: pubPEM})
	resp, err = insecureClient().Post(base+"/v1/enroll", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var enrolled api.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&enrolled); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if enrolled.Certificate == "" {
		t.Fatal("enrollment returned no certificate")
	}

	write(t, layout.IdentityKey(), key, 0o600)
	write(t, layout.IdentityCert(), enrolled.Certificate, 0o644)
	write(t, filepath.Join(layout.Data, "ca.crt"), enrolled.CA, 0o644)

	c, err := worker.DialTLS(strings.TrimPrefix(base, "https://"),
		layout.IdentityCert(), layout.IdentityKey(), filepath.Join(layout.Data, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// insecureClient talks to a control plane whose CA it has not been given.
//
// Only for the bootstrap steps a real client also performs unverified -- minting
// and redeeming, where nothing secret is at risk in this test's own process --
// and never for anything that carries an identity.
func insecureClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS12,
		}},
	}
}

func keypairPEM(t *testing.T) (keyPEM, pubPEM string) {
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

// registerWith exercises a real worker endpoint over the presented identity.
//
// Registration rather than a read, because it is the endpoint where the
// certificate actually decides something: the API replaces whatever name the
// body carries with the one the certificate proves.
func registerWith(c *worker.Client) (store.Worker, error) {
	return c.Register(context.Background(), store.Worker{
		ID: "worker-ignored", Name: "ignored",
		Labels: []string{store.DefaultLabel}, Roles: []string{store.RoleExecute},
		Version: "test",
	})
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// The renewal that matters is the one nobody asks for. A worker left running
// must replace its own certificate before it expires, or a 24-hour leaf turns
// "always on" into "breaks daily" -- which is the thing that has to be true
// before certificates can be required at all (D25).
func TestARunningWorkerRenewsItselfUnattended(t *testing.T) {
	// A leaf shorter than the renewal window, so the very first heartbeat is
	// already inside it.
	restore := compressLifetimes(t, 20*time.Second, 30*time.Second)
	defer restore()
	oldBeat := engine.HeartbeatInterval
	engine.HeartbeatInterval = 200 * time.Millisecond
	defer func() { engine.HeartbeatInterval = oldBeat }()

	base, layout := startTLSDaemon(t)
	client := enrollAWorker(t, base, layout, "unattended", []string{store.DefaultLabel})
	before := read(t, layout.IdentityCert())

	w, err := worker.New(worker.Options{
		Name: "unattended", Labels: []string{store.DefaultLabel},
		Concurrency: 1, Version: "test",
		CacheDir: layout.Data,
		Client:   client,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// One heartbeat is enough; the interval is what decides how long that is.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if read(t, layout.IdentityCert()) != before {
			return // renewed, with nobody asking
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("a running worker did not renew its own certificate before it expired")
}

// A worker on the control plane's own machine gets an identity with nobody
// asked for anything -- which is what has to be true before certificates can be
// required at all, since `je quickstart` and `docker compose up` must stay at
// zero extra steps (D25).
func TestALocalWorkerEnrollsItselfFromTheDataDirectory(t *testing.T) {
	_, layout := startTLSDaemon(t)

	token, err := os.ReadFile(layout.BootstrapToken())
	if err != nil {
		t.Fatalf("the control plane left no token for local workers: %v", err)
	}

	// The trust anchor, made explicit: this file sits in a directory whose
	// other contents include the key that signs everything.
	info, err := os.Stat(layout.BootstrapToken())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("bootstrap token mode = %o, want 0600", perm)
	}
	if _, err := os.Stat(layout.CAKey()); err != nil {
		t.Fatalf("the CA key is not beside the token, so reading it proves nothing: %v", err)
	}
	if len(strings.TrimSpace(string(token))) == 0 {
		t.Fatal("the bootstrap token is empty")
	}
}

// The token is removed when the control plane stops, so a stale file cannot
// outlive the process that honoured it.
func TestTheBootstrapTokenDoesNotOutliveTheControlPlane(t *testing.T) {
	dir := t.TempDir()
	layout := paths.Layout{Data: dir}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Config{
			Layout: layout, Addr: "127.0.0.1:0", Version: "test",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Ready:  ready,
		})
	}()
	<-ready

	if _, err := os.Stat(layout.BootstrapToken()); err != nil {
		t.Fatalf("no token while running: %v", err)
	}
	cancel()
	<-done

	if _, err := os.Stat(layout.BootstrapToken()); !os.IsNotExist(err) {
		t.Error("the bootstrap token survived the control plane that wrote it")
	}
}

// There is no plaintext listener, and a client that tries one gets nothing.
//
// The guard for D25's last step. The flip is easy to undo by accident -- a
// `serve` variable that falls back, a config field that defaults to false --
// and every other test in this file would still pass if it did, because they
// all speak TLS. This one fails.
func TestThereIsNoPlaintextListener(t *testing.T) {
	dir := t.TempDir()
	layout := paths.Layout{Data: dir}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Config{
			Layout: layout, Addr: "127.0.0.1:0", Version: "test",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Ready:  ready,
		})
	}()
	<-ready
	t.Cleanup(func() { cancel(); <-done })

	info, err := daemon.ReadRuntime(layout.Runtime())
	if err != nil {
		t.Fatal(err)
	}
	if !info.TLS {
		t.Fatal("the runtime file does not say this control plane serves TLS")
	}

	// An authority exists on every control plane now, because every control
	// plane needs a certificate of its own to serve at all.
	if _, err := os.Stat(layout.CAKey()); err != nil {
		t.Errorf("no certificate authority: %v", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + info.Address + "/v1/health")
	if err != nil {
		return // refused outright, which is also a correct answer
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a plaintext request reached the API")
	}
	// Go's TLS server answers a plaintext request in plaintext, once, to say
	// what went wrong. Worth asserting rather than tolerating: it is the
	// difference between an upgrade that explains itself and one that produces
	// a connection reset somebody has to guess at.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if !strings.Contains(string(body), "HTTPS") {
		t.Errorf("plaintext request answered %s: %s", resp.Status, body)
	}
}

// Writing requires an identity once a deployment has issued a client one, and
// reading never does (D25).
//
// The two halves are one test because the interesting property is the
// difference between them: a rule that refused everything would pass half of
// this and be useless, and one that refused nothing would pass the other half.
func TestWritingRequiresAnIdentityOnceAClientExists(t *testing.T) {
	base, layout := startTLSDaemon(t)
	anonymous := insecureClient()

	// Before any client identity exists there is nobody to be, so an
	// unidentified write is allowed -- otherwise a fresh deployment could do
	// nothing at all.
	if code := postSync(t, anonymous, base); code == http.StatusUnauthorized {
		t.Fatal("an unidentified write was refused before any client identity existed")
	}

	enrollClient(t, base, layout, "jays-laptop")

	if code := postSync(t, anonymous, base); code != http.StatusUnauthorized {
		t.Errorf("unidentified write = %d, want 401 once a client identity exists", code)
	}
	resp, err := anonymous.Get(base + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("read = %d, want 200 -- arming the gate must not close reading",
			resp.StatusCode)
	}
}

// The gate is on the HTTP method, so an endpoint added later is covered without
// anybody remembering to add it to a list.
func TestEveryWriteMethodIsGated(t *testing.T) {
	base, layout := startTLSDaemon(t)
	enrollClient(t, base, layout, "jays-laptop")

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v1/runs"},
		{http.MethodPost, "/v1/events"},
		{http.MethodPost, "/v1/sources"},
		{http.MethodDelete, "/v1/sources/anything"},
		{http.MethodPut, "/v1/secrets/TOKEN"},
		{http.MethodDelete, "/v1/secrets/TOKEN"},
		// Minting is deliberately gated: it decides what a machine may call
		// itself, and is the one write nobody unidentified should perform.
		{http.MethodPost, "/v1/enroll/tokens"},
	} {
		req, err := http.NewRequest(tc.method, base+tc.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := insecureClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, resp.StatusCode)
		}
	}

	// And the exemption still holds: redeeming a token is how a caller with no
	// identity obtains one, so it cannot require having one.
	resp, err := insecureClient().Post(base+"/v1/enroll", "application/json",
		strings.NewReader(`{"token":"not-a-real-token","public_key":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("redeeming a token was gated on already having an identity")
	}
}

// An actor is what the certificate says, not what the request body asks for.
//
// This is the D7 half of the item: "the person responsible" is only worth
// recording if it cannot be chosen by whoever is being recorded.
func TestTheActorComesFromTheCertificate(t *testing.T) {
	base, layout := startTLSDaemon(t)
	client := enrollClient(t, base, layout, "jays-laptop")

	// The body claims somebody else entirely. It must not be believed.
	body, _ := json.Marshal(api.TriggerRequest{Job: "src/hello", Actor: "somebody-else"})
	resp, err := client.Post(base+"/v1/runs", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("trigger = %d, want 202", resp.StatusCode)
	}

	var events struct {
		Events []struct {
			Type  string `json:"type"`
			Actor string `json:"actor"`
		} `json:"events"`
	}
	getInto(t, client, base+"/v1/events?limit=50", &events)

	var found bool
	for _, e := range events.Events {
		if e.Type != "run.requested" {
			continue
		}
		found = true
		if e.Actor != "jays-laptop" {
			t.Errorf("actor = %q, want %q -- the body's claim was believed",
				e.Actor, "jays-laptop")
		}
	}
	if !found {
		t.Fatal("no run.requested event was recorded")
	}
}

// enrollClient enrolls an identity carrying the client role and returns an HTTP
// client presenting it. It writes into its own directory, so the control
// plane's data directory is left as a control plane's.
func enrollClient(t *testing.T, base string, layout paths.Layout, name string) *http.Client {
	t.Helper()

	body, _ := json.Marshal(api.MintEnrollmentRequest{Name: name, Roles: []string{store.RoleClient}})
	resp, err := insecureClient().Post(base+"/v1/enroll/tokens", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var mint api.MintEnrollmentResponse
	if err := json.NewDecoder(resp.Body).Decode(&mint); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !mint.ArmsIdentity {
		t.Error("minting the first client identity did not report that it arms the gate")
	}

	key, pubPEM := keypairPEM(t)
	body, _ = json.Marshal(api.EnrollRequest{Token: mint.Token, PublicKey: pubPEM})
	resp, err = insecureClient().Post(base+"/v1/enroll", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var enrolled api.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&enrolled); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if enrolled.Certificate == "" {
		t.Fatal("client enrollment returned no certificate")
	}

	cert, err := tls.X509KeyPair([]byte(enrolled.Certificate), []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(enrolled.CA)) {
		t.Fatal("the returned authority is not a certificate")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		}},
	}
}

func postSync(t *testing.T, c *http.Client, base string) int {
	t.Helper()
	resp, err := c.Post(base+"/v1/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func getInto(t *testing.T, c *http.Client, url string, into any) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatal(err)
	}
}

// An age key is bound to an identity the control plane issued, so a recipient
// list can name a machine instead of carrying a key somebody pasted (D25).
func TestAnAgeKeyIsBoundToAnIdentity(t *testing.T) {
	base, layout := startTLSDaemon(t)

	recipient := "age1zvkyg2lqzraa2lnjvqej32nkuu0ues2s82hzrye869xeexvn73equnujwj"
	client := enrollClientWithKey(t, base, layout, "jays-laptop", recipient)

	var out api.AgeKeyResponse
	getInto(t, client, base+"/v1/identities/jays-laptop/age-key", &out)
	if out.Recipient != recipient {
		t.Errorf("resolved key = %q, want %q", out.Recipient, recipient)
	}

	// A name nobody enrolled resolves to nothing, rather than to something
	// plausible. This is the whole value of the binding: the answer is about an
	// identity this control plane issued, or there is no answer.
	resp, err := client.Get(base + "/v1/identities/nobody/age-key")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown identity = %d, want 404", resp.StatusCode)
	}
}

// Registering a key later works, and registers it for the caller -- never for
// whoever a request body names.
func TestAnAgeKeyIsRegisteredForTheCallerOnly(t *testing.T) {
	base, layout := startTLSDaemon(t)
	client := enrollClientWithKey(t, base, layout, "jays-laptop", "")

	recipient := "age1zvkyg2lqzraa2lnjvqej32nkuu0ues2s82hzrye869xeexvn73equnujwj"
	body, _ := json.Marshal(api.AgeKeyRequest{Recipient: recipient})
	resp, err := client.Post(base+"/v1/identity/age-key", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var registered api.AgeKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&registered); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if registered.Name != "jays-laptop" {
		t.Errorf("registered for %q, want the calling certificate's name", registered.Name)
	}

	// With no certificate there is nobody to register a key for, so the body
	// cannot be used to name somebody.
	resp, err = insecureClient().Post(base+"/v1/identity/age-key", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unidentified key registration = %d, want 401", resp.StatusCode)
	}
}

func enrollClientWithKey(t *testing.T, base string, layout paths.Layout, name, recipient string) *http.Client {
	t.Helper()

	body, _ := json.Marshal(api.MintEnrollmentRequest{Name: name, Roles: []string{store.RoleClient}})
	resp, err := insecureClient().Post(base+"/v1/enroll/tokens", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var mint api.MintEnrollmentResponse
	if err := json.NewDecoder(resp.Body).Decode(&mint); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	key, pubPEM := keypairPEM(t)
	body, _ = json.Marshal(api.EnrollRequest{
		Token: mint.Token, PublicKey: pubPEM, AgeRecipient: recipient,
	})
	resp, err = insecureClient().Post(base+"/v1/enroll", "application/json",
		strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var enrolled api.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&enrolled); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if enrolled.Certificate == "" {
		t.Fatal("enrollment returned no certificate")
	}

	cert, err := tls.X509KeyPair([]byte(enrolled.Certificate), []byte(key))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(enrolled.CA)) {
		t.Fatal("the returned authority is not a certificate")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		}},
	}
}
