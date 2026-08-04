package remote

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *DeviceStore {
	t.Helper()
	s, err := LoadDeviceStore(filepath.Join(t.TempDir(), "remote-devices.json"), nil)
	if err != nil {
		t.Fatalf("LoadDeviceStore: %v", err)
	}
	return s
}

func TestTokenHashingAndVerification(t *testing.T) {
	store := newTestStore(t)
	token, dev, err := store.Mint("Galaxy S25 FE")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	cases := []struct {
		name    string
		token   string
		prepare func()
		wantOK  bool
	}{
		{name: "the minted token", token: token, wantOK: true},
		{name: "empty", token: "", wantOK: false},
		{name: "wrong length", token: "short", wantOK: false},
		{
			name:   "right length, wrong value",
			token:  strings.Repeat("A", tokenChars),
			wantOK: false,
		},
		{
			name:  "one character altered",
			token: flipFirstChar(token),
			// Still the right length, so it reaches the comparison — which is
			// exactly the case a length check alone would let through.
			wantOK: false,
		},
		{
			name:    "revoked device",
			token:   token,
			prepare: func() { store.Revoke(dev.ID, "test") },
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.prepare != nil {
				tc.prepare()
			}
			_, err := store.Verify(tc.token)
			if (err == nil) != tc.wantOK {
				t.Fatalf("Verify(%q) err=%v, wantOK=%v", tc.name, err, tc.wantOK)
			}
		})
	}
}

func flipFirstChar(s string) string {
	if s == "" {
		return s
	}
	c := byte('A')
	if s[0] == 'A' {
		c = 'B'
	}
	return string(c) + s[1:]
}

// TestNoPlaintextTokenAtRest is the security claim of B2 stated as a test: the
// file may hold a hash and nothing else. A reader of remote-devices.json —
// including an agent running at this uid — learns nothing it can present.
func TestNoPlaintextTokenAtRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote-devices.json")
	store, err := LoadDeviceStore(path, nil)
	if err != nil {
		t.Fatalf("LoadDeviceStore: %v", err)
	}
	token, dev, err := store.Mint("phone")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("the plaintext pairing token was written to disk")
	}
	if !strings.Contains(string(raw), HashToken(token)) {
		t.Fatal("the token hash is missing from the store")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("store mode = %o, want 600", perm)
	}
	if dev.TokenHash != HashToken(token) {
		t.Error("the returned device does not carry the token hash")
	}
}

func TestKillSwitchBlocksTokenExchange(t *testing.T) {
	store := newTestStore(t)
	token, _, err := store.Mint("phone")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := store.Verify(token); err != nil {
		t.Fatalf("Verify before kill: %v", err)
	}
	store.SetKillSwitch(true)
	if _, err := store.Verify(token); err == nil {
		t.Fatal("token exchanged while the kill-switch was engaged")
	}
	store.SetKillSwitch(false)
	if _, err := store.Verify(token); err != nil {
		t.Fatalf("Verify after re-arm: %v", err)
	}
}

func TestSessionCookieLifecycle(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	store, err := LoadDeviceStore(filepath.Join(t.TempDir(), "d.json"), clock)
	if err != nil {
		t.Fatalf("LoadDeviceStore: %v", err)
	}
	_, dev, err := store.Mint("phone")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	value, _, err := store.NewSession(dev.ID)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	cases := []struct {
		name    string
		cookie  func() string
		prepare func()
		wantOK  bool
	}{
		{name: "fresh cookie", cookie: func() string { return value }, wantOK: true},
		{name: "empty", cookie: func() string { return "" }, wantOK: false},
		{name: "not our format", cookie: func() string { return "hello" }, wantOK: false},
		{
			name:   "tampered signature",
			cookie: func() string { return value[:len(value)-2] + "xy" },
			wantOK: false,
		},
		{
			name: "tampered device id",
			cookie: func() string {
				p := strings.Split(value, ".")
				p[1] = "d-deadbeef"
				return strings.Join(p, ".")
			},
			wantOK: false,
		},
		{
			name:    "expired",
			cookie:  func() string { return value },
			prepare: func() { now = now.Add(sessionTTL + time.Hour) },
			wantOK:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.prepare != nil {
				tc.prepare()
			}
			_, ok := store.VerifySession(tc.cookie())
			if ok != tc.wantOK {
				t.Fatalf("VerifySession(%s) = %v, want %v", tc.name, ok, tc.wantOK)
			}
		})
	}
}

// TestRevokeInvalidatesOutstandingCookies is the reason the device record carries
// an epoch: the cookie is a stateless HMAC that must survive a core restart, so
// it cannot be invalidated by forgetting it.
func TestRevokeInvalidatesOutstandingCookies(t *testing.T) {
	store := newTestStore(t)
	_, dev, err := store.Mint("phone")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	value, _, err := store.NewSession(dev.ID)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, ok := store.VerifySession(value); !ok {
		t.Fatal("fresh cookie did not verify")
	}
	store.Revoke(dev.ID, "test")
	if _, ok := store.VerifySession(value); ok {
		t.Fatal("a cookie minted before the revoke still verifies")
	}
}

// TestSessionSurvivesReload proves the property the whole stateless-cookie
// design exists for: the core exits whenever the desktop app closes, and a phone
// no longer holds its pairing token, so a session that died with the process
// would mean re-pairing after every restart.
func TestSessionSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	first, err := LoadDeviceStore(path, nil)
	if err != nil {
		t.Fatalf("LoadDeviceStore: %v", err)
	}
	_, dev, err := first.Mint("phone")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	value, _, err := first.NewSession(dev.ID)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	second, err := LoadDeviceStore(path, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := second.VerifySession(value); !ok {
		t.Fatal("the session cookie did not survive a store reload")
	}
}

func TestAuthMiddlewareRejectsUnauthenticatedAndRevoked(t *testing.T) {
	env := newTestEnv(t)

	protected := []struct{ method, path string }{
		{"GET", "/api/v1/meta"},
		{"GET", "/api/v1/agents"},
		{"GET", "/api/v1/agents/t-1"},
		{"GET", "/api/v1/agents/t-1/transcript"},
		{"POST", "/api/v1/agents/t-1/interrupt"},
		{"POST", "/api/v1/agents/t-1/stop"},
		{"GET", "/api/v1/agents/t-1/diff"},
		{"GET", "/api/v1/agents/t-1/git/status"},
		{"GET", "/api/v1/agents/t-1/git/log"},
		{"POST", "/api/v1/permissions/perm-1"},
		{"GET", "/api/v1/events"},
		{"POST", "/api/v1/auth/logout"},
	}

	t.Run("no cookie", func(t *testing.T) {
		for _, r := range protected {
			resp := env.do(r.method, r.path, `{}`, nil)
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s unauthenticated = %d, want 401", r.method, r.path, resp.StatusCode)
				continue
			}
			if body["error"] != "unauthenticated" {
				t.Errorf("%s %s error code = %v, want unauthenticated", r.method, r.path, body["error"])
			}
		}
	})

	t.Run("garbage cookie", func(t *testing.T) {
		junk := "v1.d-nope.9999999999.nonce.0.bad"
		for _, r := range protected {
			resp := env.do(r.method, r.path, `{}`, &junk)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s forged cookie = %d, want 401", r.method, r.path, resp.StatusCode)
			}
		}
	})

	t.Run("after revoke", func(t *testing.T) {
		if code, _ := env.authJSON("GET", "/api/v1/meta", ""); code != http.StatusOK {
			t.Fatalf("meta before revoke = %d", code)
		}
		env.srv.RevokeDevice(env.device.ID, "test")
		for _, r := range protected {
			resp := env.auth(r.method, r.path, `{}`)
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s after revoke = %d, want 401", r.method, r.path, resp.StatusCode)
			}
		}
	})
}

func TestAuthExchangeSetsHardenedCookie(t *testing.T) {
	env := newTestEnv(t)
	token, _, _, err := env.srv.MintDevice("second phone")
	if err != nil {
		t.Fatalf("MintDevice: %v", err)
	}
	resp := env.do("POST", "/api/v1/auth/exchange", `{"token":"`+token+`"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exchange = %d", resp.StatusCode)
	}
	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			found = c
		}
	}
	if found == nil {
		t.Fatal("no session cookie")
	}
	if !found.Secure {
		t.Error("cookie is not Secure")
	}
	if !found.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if found.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie SameSite = %v, want Strict", found.SameSite)
	}
	if found.Path != "/" {
		t.Errorf("cookie Path = %q, want /", found.Path)
	}
	if found.MaxAge != int(sessionTTL/time.Second) {
		t.Errorf("cookie Max-Age = %d, want %d", found.MaxAge, int(sessionTTL/time.Second))
	}
}

func TestAuthExchangeRejectsBadTokenUniformly(t *testing.T) {
	env := newTestEnv(t)
	cases := []struct{ name, body string }{
		{"unknown token", `{"token":"` + strings.Repeat("A", tokenChars) + `"}`},
		{"empty token", `{"token":""}`},
		{"not json", `nope`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := env.do("POST", "/api/v1/auth/exchange", tc.body, nil)
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatal("a bad exchange succeeded")
			}
			for _, c := range resp.Cookies() {
				if c.Name == cookieName && c.Value != "" {
					t.Fatal("a failed exchange set a session cookie")
				}
			}
		})
	}
}

func TestAuthExchangeIsRateLimited(t *testing.T) {
	env := newTestEnv(t)
	bad := `{"token":"` + strings.Repeat("A", tokenChars) + `"}`
	sawLimit := false
	// authRateBurst attempts are allowed; the next one must be refused. A couple
	// of extra iterations keep the test honest if the burst is ever raised.
	for i := 0; i < authRateBurst+3; i++ {
		resp := env.do("POST", "/api/v1/auth/exchange", bad, nil)
		code := resp.StatusCode
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if code == http.StatusTooManyRequests {
			if body["error"] != "rate-limited" {
				t.Fatalf("rate-limited error code = %v", body["error"])
			}
			sawLimit = true
			break
		}
	}
	if !sawLimit {
		t.Fatalf("no rate limit after %d failed exchanges", authRateBurst+3)
	}
}

func TestRateLimiterRefills(t *testing.T) {
	now := time.Now()
	l := newRateLimiter(func() time.Time { return now })
	for i := 0; i < authRateBurst; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("attempt %d refused inside the burst", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("burst was not enforced")
	}
	if !l.allow("5.6.7.8") {
		t.Fatal("a different source was penalised for another's burst")
	}
	now = now.Add(authRateWindow)
	if !l.allow("1.2.3.4") {
		t.Fatal("the bucket never refilled")
	}
}

func TestCorruptStoreFailsClosedWithoutReplacingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := LoadDeviceStore(path, nil)
	if err == nil {
		t.Fatal("a corrupt store loaded without complaint")
	}
	if store != nil {
		t.Fatal("a corrupt store returned a usable credential table")
	}
	if raw, statErr := os.ReadFile(path); statErr != nil || string(raw) != "{not json" {
		t.Fatalf("the corrupt store was changed during read: %q / %v", raw, statErr)
	}
}

func TestDeviceCapabilitiesAreOptInAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	store, err := LoadDeviceStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, dev, err := store.Mint("developer phone")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Allows(CapAgentManage) || dev.Allows(CapWorktreeView) {
		t.Fatal("a newly paired device unexpectedly has developer powers")
	}
	updated, changed, err := store.SetCapabilities(dev.ID, []Capability{CapWorktreeView, CapAgentManage, CapWorktreeView})
	if err != nil || !changed {
		t.Fatalf("SetCapabilities = %#v, %v, %v", updated, changed, err)
	}
	if !updated.Allows(CapAgentManage) || !updated.Allows(CapWorktreeView) || updated.Allows(CapWorktreeEdit) {
		t.Fatalf("capabilities = %#v", updated.Capabilities)
	}
	reloaded, err := LoadDeviceStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get(dev.ID)
	if !ok || !got.Allows(CapAgentManage) || !got.Allows(CapWorktreeView) {
		t.Fatalf("persisted device = %#v, present=%v", got, ok)
	}
	if _, _, err := reloaded.SetCapabilities(dev.ID, []Capability{"not-a-capability"}); err == nil {
		t.Fatal("unknown capability was accepted")
	}
}
