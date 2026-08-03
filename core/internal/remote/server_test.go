package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBindRefusesWildcardWithoutExplicitOptIn(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		allow   bool
		wantErr bool
	}{
		{name: "loopback", addr: "127.0.0.1:0"},
		{name: "wildcard v4 without opt-in", addr: "0.0.0.0:0", wantErr: true},
		{name: "wildcard v6 without opt-in", addr: "[::]:0", wantErr: true},
		{name: "bare port without opt-in", addr: ":0", wantErr: true},
		{name: "wildcard with opt-in", addr: "127.0.0.1:0", allow: true},
		{name: "not host:port", addr: "nonsense", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(Config{
				BindAddr:           tc.addr,
				AllowAllInterfaces: tc.allow,
				DataDir:            t.TempDir(),
				Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
			}, &fakeBackend{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = srv.Start(context.Background())
			if err == nil {
				t.Cleanup(func() { _ = srv.Stop(context.Background()) })
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("Start(%q) err = %v, wantErr %v", tc.addr, err, tc.wantErr)
			}
			if tc.wantErr && err != nil && strings.Contains(tc.addr, "0.0.0") &&
				!strings.Contains(err.Error(), "all interfaces") {
				t.Errorf("wildcard refusal did not explain itself: %v", err)
			}
		})
	}
}

// TestServesOverTLSWithAVerifiableCertificate is the mobile-browser
// prerequisite: a phone rejects a certificate whose subjectAltName does not
// contain the exact host in its address bar, and on a LAN that host is an IP.
func TestServesOverTLSWithAVerifiableCertificate(t *testing.T) {
	dir := t.TempDir()
	srv, err := New(Config{
		BindAddr: "127.0.0.1:0",
		DataDir:  dir,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, &fakeBackend{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop(context.Background()) }()

	if srv.CertFingerprint() == "" {
		t.Error("no certificate fingerprint to show the user for comparison")
	}
	if !strings.Contains(srv.CertFingerprint(), ":") {
		t.Errorf("fingerprint %q is not in the colon-separated form a browser shows",
			srv.CertFingerprint())
	}

	pem, err := os.ReadFile(filepath.Join(dir, certFileName))
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("generated certificate is not valid PEM")
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	// A VERIFYING client: if the SAN did not cover 127.0.0.1 this fails.
	resp, err := client.Get("https://" + srv.Addr() + "/api/v1/meta")
	if err != nil {
		t.Fatalf("TLS request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated meta over TLS = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
		t.Errorf("CSP missing on a TLS response: %q", got)
	}
}

func TestCertificateCoversTheChosenBindAddress(t *testing.T) {
	cases := []struct {
		name string
		host string
		want []string
	}{
		{name: "lan ip", host: "192.168.1.20", want: []string{"192.168.1.20", "127.0.0.1", "localhost"}},
		{name: "loopback", host: "127.0.0.1", want: []string{"127.0.0.1", "localhost"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cert, fp, err := ensureCert(dir, tc.host, time.Now())
			if err != nil {
				t.Fatalf("ensureCert: %v", err)
			}
			if fp == "" {
				t.Error("no fingerprint")
			}
			for _, host := range tc.want {
				if err := cert.Leaf.VerifyHostname(host); err != nil {
					t.Errorf("certificate does not cover %q: %v", host, err)
				}
			}
			if info, err := os.Stat(filepath.Join(dir, keyFileName)); err != nil {
				t.Errorf("key not written: %v", err)
			} else if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("key mode = %o, want 600", perm)
			}
		})
	}
}

// TestCertificateIsRegeneratedForANewNetwork covers the case that actually
// happens: the laptop moves, the LAN address changes, and yesterday's
// certificate no longer matches what the phone types.
func TestCertificateIsRegeneratedForANewNetwork(t *testing.T) {
	dir := t.TempDir()
	first, _, err := ensureCert(dir, "192.168.1.20", time.Now())
	if err != nil {
		t.Fatalf("ensureCert: %v", err)
	}
	second, _, err := ensureCert(dir, "10.0.0.5", time.Now())
	if err != nil {
		t.Fatalf("ensureCert (new network): %v", err)
	}
	if err := second.Leaf.VerifyHostname("10.0.0.5"); err != nil {
		t.Fatalf("regenerated certificate does not cover the new address: %v", err)
	}
	if first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) == 0 {
		t.Fatal("the certificate was reused for an address it does not cover")
	}
	// And a matching address must NOT churn the certificate — a new cert means a
	// new trust-on-first-use prompt on every phone.
	third, _, err := ensureCert(dir, "10.0.0.5", time.Now())
	if err != nil {
		t.Fatalf("ensureCert (same network): %v", err)
	}
	if third.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) != 0 {
		t.Fatal("the certificate was regenerated even though it still matched")
	}
}

func TestCertificateIsRenewedBeforeItExpires(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	first, _, err := ensureCert(dir, "127.0.0.1", now)
	if err != nil {
		t.Fatalf("ensureCert: %v", err)
	}
	// Inside the renew window: the cert is still valid, but not for long enough.
	later := now.Add(certValidity - certRenewWindow/2)
	second, _, err := ensureCert(dir, "127.0.0.1", later)
	if err != nil {
		t.Fatalf("ensureCert (near expiry): %v", err)
	}
	if first.Leaf.SerialNumber.Cmp(second.Leaf.SerialNumber) == 0 {
		t.Fatal("a certificate about to expire was not renewed")
	}
}

// TestNilServerIsSafeToPublishInto is what lets the core's fan-out call these
// unconditionally. A nil check the caller can forget is a nil check that will be
// forgotten, on the one path that runs for every event of every agent.
func TestNilServerIsSafeToPublishInto(t *testing.T) {
	var s *Server
	s.PublishTurnState(TurnState{ThreadID: "t-1"})
	s.PublishPermissionRequested(PermissionRequested{ThreadID: "t-1", RequestID: "p-1"})
	s.PublishPermissionResolved(PermissionResolved{ThreadID: "t-1", RequestID: "p-1"})
	s.PublishTranscript("t-1", []TranscriptEvent{{Kind: "assistant"}})
	s.PublishAgentGone("t-1", "exited")
	s.SetKillSwitch(true)
	s.RevokeDevice("d-1", "x")
	if s.Running() || s.Addr() != "" || s.CertFingerprint() != "" ||
		s.KillSwitch() || s.AuditTampered() || len(s.Devices()) != 0 {
		t.Fatal("a nil server reported state it does not have")
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop on a nil server: %v", err)
	}
}

func TestRosterEventIsCoalescedAndDerivedFromTheBackend(t *testing.T) {
	be := &fakeBackend{agents: sampleAgents()}
	env := newTestEnvWith(t, be)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go env.srv.rosterLoop(ctx)

	c := env.openSSE("?scope=roster", -1)
	c.skipRetry()
	c.next() // hello

	// Three flips in a burst must produce one roster snapshot, not three.
	for i := 0; i < 3; i++ {
		env.srv.PublishTurnState(TurnState{ThreadID: "t-1", Busy: i%2 == 0, Attention: true})
	}
	for i := 0; i < 3; i++ {
		if f := c.next(); f.event != evTurnState {
			t.Fatalf("frame %d = %q, want turnState", i, f.event)
		}
	}
	f := c.next()
	if f.event != evRoster {
		t.Fatalf("frame after the burst = %q, want roster", f.event)
	}
	body := decodeData(t, f)
	rows, _ := body["agents"].([]any)
	if len(rows) != 2 {
		t.Fatalf("roster event carried %d rows", len(rows))
	}
	listCalls := 0
	for _, call := range be.callLog() {
		if call == "ListAgents" {
			listCalls++
		}
	}
	if listCalls != 1 {
		t.Fatalf("the backend was polled %d times for one burst of flips", listCalls)
	}
}

func TestThreadScopeGetsItsOwnEventsAndNoRosterBody(t *testing.T) {
	env := newTestEnvWith(t, &fakeBackend{agents: sampleAgents()})
	c := env.openSSE("?scope=thread&threadId=t-1", -1)
	c.skipRetry()
	c.next() // hello

	// Published while a viewer is attached, so it reaches the ring.
	env.srv.PublishTranscript("t-1", []TranscriptEvent{{Kind: "assistant", Text: "safe"}})
	// Another thread's traffic must not reach this subscriber at all.
	env.srv.PublishTranscript("t-2", []TranscriptEvent{{Kind: "assistant", Text: "safe"}})
	// A roster body is roster-scope only.
	env.srv.hub.publish(evRoster, "", []byte(`{"agents":[]}`))
	// A prompt on ANOTHER thread still reaches it: the phone must learn that a
	// different agent is parked while it reads this chat.
	env.srv.PublishPermissionRequested(PermissionRequested{
		ThreadID: "t-2", RequestID: "p-1", Kind: "tool", ToolName: "Bash",
		Summary: "Run: ls", Deadline: time.Now().Add(time.Minute),
	})

	f := c.next()
	if f.event != evAgentEvent {
		t.Fatalf("first frame = %q, want agentEvent", f.event)
	}
	if got := decodeData(t, f)["threadId"]; got != "t-1" {
		t.Fatalf("agentEvent threadId = %v", got)
	}
	f = c.next()
	if f.event != evPermissionRequested {
		t.Fatalf("second frame = %q; another thread's per-event traffic or the "+
			"roster body leaked into a thread subscription", f.event)
	}
}

// TestPermissionEventCarriesNoRawInput is the redaction rule expressed as a
// type: PermissionRequested has no input field, so there is nothing to leak.
func TestPermissionEventCarriesNoRawInput(t *testing.T) {
	env := newTestEnv(t)
	c := env.openSSE("?scope=roster", -1)
	c.skipRetry()
	c.next() // hello

	env.srv.PublishPermissionRequested(PermissionRequested{
		ThreadID: "t-1", RequestID: "p-1", Kind: "tool", ToolName: "Bash",
		Summary:  "Run: npm ci",
		Deadline: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	})
	f := c.next()
	if f.event != evPermissionRequested {
		t.Fatalf("frame = %q", f.event)
	}
	body := decodeData(t, f)
	for _, forbidden := range []string{"input", "arguments", "plan", "questions"} {
		if _, present := body[forbidden]; present {
			t.Errorf("permissionRequested carried %q; raw tool input must never leave "+
				"this machine", forbidden)
		}
	}
	if body["deadline"] != "2026-07-31T12:00:00Z" {
		t.Errorf("deadline = %v, want an absolute RFC 3339 UTC instant", body["deadline"])
	}
	if body["serverTime"] == nil {
		t.Error("no serverTime beside the deadline; the client cannot derive its offset")
	}
	if _, present := body["timeoutSeconds"]; present {
		t.Error("timeoutSeconds leaked onto the remote surface; deadlines are absolute")
	}
}

func TestLongSummaryIsClippedOnTheWire(t *testing.T) {
	env := newTestEnv(t)
	c := env.openSSE("?scope=roster", -1)
	c.skipRetry()
	c.next()

	long := strings.Repeat("x", maxSummaryBytes*3)
	env.srv.PublishPermissionRequested(PermissionRequested{
		ThreadID: "t-1", RequestID: "p-1", Summary: long,
	})
	body := decodeData(t, c.next())
	got, _ := body["summary"].(string)
	if len(got) > maxSummaryBytes+len("…") {
		t.Fatalf("summary is %d bytes; the core does not bound it, so the wire must",
			len(got))
	}
}

func TestStopIsIdempotentAndClosesStreams(t *testing.T) {
	srv, err := New(Config{
		BindAddr: "127.0.0.1:0",
		DataDir:  t.TempDir(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, &fakeBackend{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !srv.Running() {
		t.Fatal("Running() is false after a successful Start")
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if srv.Running() {
		t.Fatal("Running() is true after Stop")
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	// And it can come back up.
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	_ = srv.Stop(context.Background())
}

func TestPairingURLPutsTheTokenInTheFragment(t *testing.T) {
	env := newTestEnv(t)
	token, pairing, dev, err := env.srv.MintDevice("Galaxy S25 FE")
	if err != nil {
		t.Fatalf("MintDevice: %v", err)
	}
	if dev.Name != "Galaxy S25 FE" {
		t.Errorf("device name = %q", dev.Name)
	}
	if !strings.Contains(pairing, "#t="+token) {
		t.Fatalf("pairing URL %q does not carry the token in the fragment; a query "+
			"string would reach access logs and Referer headers", pairing)
	}
	if strings.Contains(strings.SplitN(pairing, "#", 2)[0], token) {
		t.Fatal("the token appears before the fragment")
	}
	if !strings.HasPrefix(pairing, "https://") {
		t.Errorf("pairing URL is not https: %q", pairing)
	}
}

// TestMintRefusesWhileNothingIsListening is a regression for a dogfooding
// failure: the listener refused to start (its port was already held by another
// program), and pairing STILL succeeded — printing a QR code whose URL named
// that port. Scanning it would have opened whatever else was listening there.
//
// A pairing URL is only meaningful with a listener behind it, so the two are
// tied together rather than left to each caller to remember.
func TestMintRefusesWhileNothingIsListening(t *testing.T) {
	// A fully constructed server that has never bound anything — exactly the
	// state left behind by a Start() that failed on EADDRINUSE.
	srv, err := New(Config{
		BindAddr:    "192.168.2.25:8443",
		DataDir:     t.TempDir(),
		CoreVersion: "0.test",
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, &fakeBackend{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := srv.Addr(); got != "" {
		t.Fatalf("Addr on an unstarted server = %q, want empty", got)
	}

	if got := srv.PairingURL("tok"); got != "" {
		t.Fatalf("PairingURL with no listener = %q, want empty — it must not fall "+
			"back to the REQUESTED address, which this process does not own", got)
	}
	if _, _, _, err := srv.MintDevice("phone"); !errors.Is(err, ErrNotListening) {
		t.Fatalf("MintDevice with no listener err = %v, want ErrNotListening", err)
	}
	// A refused pairing must not have recorded a device either, or the panel
	// lists a phone that was never given a way in.
	if devs := srv.Devices(); len(devs) != 0 {
		t.Fatalf("a refused pairing still recorded %d device(s)", len(devs))
	}
}
