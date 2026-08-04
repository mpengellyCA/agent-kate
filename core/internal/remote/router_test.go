package remote

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// backendMethodNames reflects the Backend interface's method set.
func backendMethodNames() []string {
	t := reflect.TypeOf((*Backend)(nil)).Elem()
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	return out
}

// frozenRoutes is the ENTIRE route set of the remote server, verbatim.
//
// This literal is the endpoint allowlist. Adding a route without adding it here
// fails TestRouteAllowlistIsFrozen, which is the point: the frozen /api/v1/
// contract is what lets a Vue app and a C++ pairing panel be built in parallel
// against a document, and a route that appears without anyone noticing is how
// that stops being true. Removing or renaming one fails too — within v1 changes
// are additive only, because a phone's cached PWA can be older than the core.
var frozenRoutes = []string{
	"POST /api/v1/auth/exchange",
	"POST /api/v1/auth/logout",
	"GET /api/v1/meta",
	"GET /api/v1/agents",
	"GET /api/v1/agents/{threadId}",
	"GET /api/v1/agents/{threadId}/transcript",
	"POST /api/v1/agents/{threadId}/send",
	"POST /api/v1/agents/{threadId}/fork",
	"POST /api/v1/agents/{threadId}/new",
	"GET /api/v1/agents/{threadId}/files",
	"GET /api/v1/agents/{threadId}/file",
	"PUT /api/v1/agents/{threadId}/file",
	"POST /api/v1/agents/{threadId}/interrupt",
	"POST /api/v1/agents/{threadId}/stop",
	"GET /api/v1/agents/{threadId}/diff",
	"GET /api/v1/agents/{threadId}/git/status",
	"GET /api/v1/agents/{threadId}/git/log",
	"POST /api/v1/permissions/{requestId}",
	// The 2026-07-31 amendment. Additive within v1, and reviewed on the way in:
	// it exposes a plan's markdown and a question's options, and nothing else.
	"GET /api/v1/permissions/{requestId}",
	"GET /api/v1/events",
	"/api/",
	"/",
}

// uiGatedMethods is the exact list of IPC methods behind ipc.Server.RequireUI —
// the eleven handlers in cmd/akcore/cowork.go that reach requireUI(d, ctx).
//
// The phone must never hold role == "ui", so none of these may be reachable over
// HTTP by any spelling. Asserting the method NAMES rather than grepping for the
// string "cowork" is deliberate: the property being defended is "the UI-gated
// set", and that set is defined by RequireUI, not by a package name.
var uiGatedMethods = []string{
	"cowork.respondGrant",
	"cowork.requestGrant",
	"cowork.listGrants",
	"cowork.revokeGrant",
	"cowork.killSwitch",
	"cowork.listAudit",
	"cowork.setEnabled",
	"cowork.portalResult",
	"cowork.getPolicy",
	"cowork.setPolicy",
	"cowork.setPointerBounds",
}

func TestRouteAllowlistIsFrozen(t *testing.T) {
	// RoutePatterns is static contract data. Keeping this assertion listener-free
	// also lets it run in restricted build sandboxes where httptest cannot open
	// a loopback socket.
	got := (&Server{}).RoutePatterns()

	if len(got) != len(frozenRoutes) {
		t.Fatalf("route count changed: got %d routes %v, frozen list has %d",
			len(got), got, len(frozenRoutes))
	}
	frozen := make(map[string]bool, len(frozenRoutes))
	for _, p := range frozenRoutes {
		frozen[p] = true
	}
	for _, p := range got {
		if !frozen[p] {
			t.Errorf("route %q is registered but not in the frozen allowlist; "+
				"if this is intentional, add it to frozenRoutes and document the "+
				"reviewed contract alongside the Remote Access integration audit", p)
		}
		delete(frozen, p)
	}
	for p := range frozen {
		t.Errorf("route %q is in the frozen allowlist but no longer registered; "+
			"removals inside /api/v1/ break phones running a cached PWA", p)
	}
}

func TestNoCoworkVerbIsReachable(t *testing.T) {
	env := newTestEnv(t)

	// Every plausible spelling an eventual mapping layer might have used. All
	// must 404, and none may be answered by the SPA fallback (which would hand a
	// caller a 200 and an index.html).
	shapes := []string{
		"/api/v1/%s",
		"/api/v1/cowork/%s",
		"/api/v1/agents/t-1/%s",
		"/api/%s",
	}
	for _, method := range uiGatedMethods {
		short := strings.TrimPrefix(method, "cowork.")
		for _, shape := range shapes {
			for _, name := range []string{method, short} {
				path := strings.Replace(shape, "%s", name, 1)
				for _, verb := range []string{"GET", "POST"} {
					resp := env.auth(verb, path, "")
					body := resp.StatusCode
					resp.Body.Close()
					if body != http.StatusNotFound &&
						body != http.StatusMethodNotAllowed {
						t.Errorf("%s %s answered %d; every UI-gated verb must be unreachable",
							verb, path, body)
					}
				}
			}
		}
	}
}

// TestBackendInterfaceHasNoPrivilegedVerb pins the structural half of the same
// property: the phone cannot reach a capability the server has no way to call.
func TestBackendInterfaceHasNoPrivilegedVerb(t *testing.T) {
	// A compile-time assertion that fakeBackend satisfies Backend, plus a
	// hand-maintained list of the method set. If someone adds a method to
	// Backend they must add it here, which is the moment to ask whether a phone
	// should be able to reach it.
	var _ Backend = (*fakeBackend)(nil)
	// reflect reports interface methods in sorted order.
	want := []string{
		"Diff", "Fork", "GitLog", "GitStatus", "Interrupt", "ListAgents", "ListFiles",
		"PermissionDetail", "ReadFile", "RespondPermission", "Send", "StartProjectAgent", "Stop", "Transcript", "WriteFile",
	}
	got := backendMethodNames()
	if len(got) != len(want) {
		t.Fatalf("Backend method set changed: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Backend method %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		name       string
		method     string
		path       string
		cookie     bool
		wantStatus int
		wantStore  bool
	}{
		{"authenticated api", "GET", "/api/v1/meta", true, http.StatusOK, true},
		{"unauthenticated api", "GET", "/api/v1/agents", false, http.StatusUnauthorized, true},
		{"unknown api path", "GET", "/api/v1/nope", true, http.StatusNotFound, true},
		{"spa fallback", "GET", "/a/t-1", true, http.StatusNotFound, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.cookie {
				resp = env.auth(tc.method, tc.path, "")
			} else {
				resp = env.do(tc.method, tc.path, "", nil)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("Content-Security-Policy"); got != contentSecurityPolicy {
				t.Errorf("CSP =\n  %q\nwant\n  %q", got, contentSecurityPolicy)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q", got)
			}
			if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q", got)
			}
			if got := resp.Header.Get("X-AgentKate-Api"); got != "1" {
				t.Errorf("X-AgentKate-Api = %q", got)
			}
			gotStore := resp.Header.Get("Cache-Control") == "no-store"
			if gotStore != tc.wantStore {
				t.Errorf("Cache-Control no-store = %v, want %v (%q)",
					gotStore, tc.wantStore, resp.Header.Get("Cache-Control"))
			}
		})
	}
}

// TestCSPStringIsExact guards the one directive that is not negotiable. A
// substring assertion would pass against `script-src 'self' 'unsafe-inline'`.
func TestCSPStringIsExact(t *testing.T) {
	const want = "default-src 'self'; script-src 'self'; object-src 'none'; " +
		"frame-ancestors 'none'; base-uri 'none'; style-src 'self' 'unsafe-inline'"
	if contentSecurityPolicy != want {
		t.Fatalf("CSP =\n  %q\nwant\n  %q", contentSecurityPolicy, want)
	}
}

// TestKillSwitchAnswers503NotAnUnpairedLie is a regression for a bug
// scripts/smoke-remote.py caught: the kill switch DID close the surface, but by
// way of VerifySession returning false, so a killed device got
// 401 "This device is not paired, or its access was revoked".
//
// That is wrong on both counts — the device IS paired and was NOT revoked, the
// whole surface was switched off — and telling someone their phone was unpaired
// when it was not is how a panic button turns into a support question.
func TestKillSwitchAnswers503NotAnUnpairedLie(t *testing.T) {
	e := newTestEnv(t)
	e.pair("phone")

	// Sanity: the session works before the switch, or the test proves nothing.
	if resp := e.auth(http.MethodGet, "/api/v1/meta", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-kill GET /api/v1/meta = %d, want 200", resp.StatusCode)
	}

	e.srv.SetKillSwitch(true)

	status, body := e.authJSON(http.MethodGet, "/api/v1/meta", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("killed GET /api/v1/meta = %d, want 503", status)
	}
	if body["error"] != "disabled" {
		t.Errorf("error = %v, want \"disabled\"", body["error"])
	}
	msg, _ := body["message"].(string)
	for _, wrong := range []string{"not paired", "revoked"} {
		if strings.Contains(strings.ToLower(msg), wrong) {
			t.Errorf("the kill-switch message claims %q, which is false: %q", wrong, msg)
		}
	}

	// Static assets keep serving so the web UI can say "switched off at the
	// desktop" rather than showing the browser's connection-failed page.
	if resp := e.auth(http.MethodGet, "/", ""); resp.StatusCode == http.StatusServiceUnavailable {
		t.Error("the kill switch also killed the shell; the UI cannot then explain itself")
	}

	// Re-arming restores the surface — a panic button that cannot be released
	// is a reinstall.
	e.srv.SetKillSwitch(false)
	if resp := e.auth(http.MethodGet, "/api/v1/meta", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("after re-arming, GET /api/v1/meta = %d, want 200", resp.StatusCode)
	}
}
