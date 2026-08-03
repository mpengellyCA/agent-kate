package remote

import (
	"encoding/json"
	"net/http"
	"strings"
)

// contentSecurityPolicy is served on EVERY response, byte for byte.
//
// script-src 'self' is the load-bearing directive and is not negotiable: the web
// UI renders agent output, agent output is attacker-influenceable, and in a
// browser holding a session cookie one XSS is full remote control of every agent
// on this machine. style-src carries 'unsafe-inline' because Vue's :style
// binding emits inline style attributes — a scoped, deliberate concession that
// buys an attacker presentation, not execution.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; object-src 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'; style-src 'self' 'unsafe-inline'"

// maxRequestBytes caps any request body. Every documented body is a handful of
// fields; 64 KiB leaves room for an ExitPlanMode plan riding back in
// updatedInput and stops an authenticated device from making us buffer a gigabyte.
const maxRequestBytes = 64 * 1024

// route is one entry in the frozen route table.
type route struct {
	// Pattern is a Go 1.22 ServeMux pattern, including the method where one
	// applies. It is compared verbatim by the endpoint allowlist test.
	Pattern string
	// Auth is false only for the pairing exchange and the two fallbacks.
	Auth    bool
	Handler http.HandlerFunc
}

// routes is THE route table. It is exhaustive, it is asserted verbatim by
// TestRouteAllowlistIsFrozen, and adding an entry without updating that test's
// literal list is a build-green/test-red event on purpose.
//
// What is absent matters as much as what is present. There is no Cowork verb and
// there never may be: a phone that could answer a grant prompt would be able to
// hand an agent screen capture and input injection, which inverts the consent
// model the entire Cowork design rests on. There is no agent creation (it needs
// project/worktree/model/provider selection and KWallet key resolution, all
// UI-resident), nothing that takes a filesystem path, and nothing destructive
// beyond a graceful stop.
func (s *Server) routes() []route {
	return []route{
		{Pattern: "POST /api/v1/auth/exchange", Auth: false, Handler: s.handleAuthExchange},
		{Pattern: "POST /api/v1/auth/logout", Auth: true, Handler: s.handleAuthLogout},
		{Pattern: "GET /api/v1/meta", Auth: true, Handler: s.handleMeta},
		{Pattern: "GET /api/v1/agents", Auth: true, Handler: s.handleAgents},
		{Pattern: "GET /api/v1/agents/{threadId}", Auth: true, Handler: s.handleAgent},
		{Pattern: "GET /api/v1/agents/{threadId}/transcript", Auth: true, Handler: s.handleTranscript},
		{Pattern: "POST /api/v1/agents/{threadId}/interrupt", Auth: true, Handler: s.handleInterrupt},
		{Pattern: "POST /api/v1/agents/{threadId}/stop", Auth: true, Handler: s.handleStop},
		{Pattern: "GET /api/v1/agents/{threadId}/diff", Auth: true, Handler: s.handleDiff},
		{Pattern: "GET /api/v1/agents/{threadId}/git/status", Auth: true, Handler: s.handleGitStatus},
		{Pattern: "GET /api/v1/agents/{threadId}/git/log", Auth: true, Handler: s.handleGitLog},
		{Pattern: "POST /api/v1/permissions/{requestId}", Auth: true, Handler: s.handlePermission},
		{Pattern: "GET /api/v1/permissions/{requestId}", Auth: true, Handler: s.handlePermissionDetail},
		{Pattern: "GET /api/v1/events", Auth: true, Handler: s.handleEvents},

		// /api/ must be claimed explicitly so an unknown API path answers with the
		// JSON error envelope instead of falling through to the SPA and handing a
		// phone an index.html it will try to parse as JSON. It also means a Cowork
		// verb spelled as a URL gets a 404, not a single-page app.
		{Pattern: "/api/", Auth: false, Handler: s.handleAPINotFound},

		// The SPA fallback. history-mode routes (/a/:id) must serve index.html;
		// with no static handler configured this is a plain 404, which is the
		// state of the world until the web UI lane lands.
		{Pattern: "/", Auth: false, Handler: s.handleStatic},
	}
}

// RoutePatterns returns the router's route set. The endpoint allowlist test
// compares this against a frozen literal.
func (s *Server) RoutePatterns() []string {
	rs := s.routes()
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Pattern)
	}
	return out
}

func (s *Server) buildRouter() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		h := r.Handler
		if r.Auth {
			h = s.requireSession(h)
		}
		mux.Handle(r.Pattern, h)
	}
	return s.securityHeaders(mux)
}

// securityHeaders applies the headers the contract puts on EVERY response, then
// the API-only ones. It wraps the mux rather than each handler so a handler
// added without thinking about headers still gets them.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-AgentKate-Api", "1")
		if isAPIPath(r.URL.Path) {
			// no-store on the API only: the web UI's hashed assets are meant to
			// be cached, and a PWA that re-downloads its bundle on every launch
			// over a phone connection is a feature nobody uses twice.
			h.Set("Cache-Control", "no-store")
		}
		// Deliberately no HSTS. The certificate is self-signed and trusted on
		// first use; pinning the origin to HTTPS-only via HSTS would turn a
		// certificate change into a lockout the user cannot click through.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

		// The kill switch answers before the auth gate, and answers 503.
		//
		// Without this the switch still closed the surface, but by way of
		// VerifySession returning false — so a killed device got
		// 401 "This device is not paired, or its access was revoked", which is
		// a lie on both counts: it IS paired and it was NOT revoked, the whole
		// surface was shut off. Telling someone their phone was unpaired when
		// it was not is how a panic button turns into a support question.
		//
		// The listener stays up and static assets keep serving, so the web UI
		// can render "remote access is switched off" rather than the browser's
		// own connection-failed page. The shell carries no agent data.
		if isAPIPath(r.URL.Path) && s.KillSwitch() {
			writeError(w, http.StatusServiceUnavailable, "disabled",
				"Remote access is switched off at the desktop. Turn it back on there to continue.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAPIPath(p string) bool { return strings.HasPrefix(p, "/api/") }

// sessionCtxKey carries the verified session to handlers.
type sessionCtxKey struct{}

func principalOf(r *http.Request) Principal {
	s := sessionOf(r.Context())
	return Principal{DeviceID: s.DeviceID, DeviceName: s.DeviceName, SessionID: s.ID}
}

// requireSession is the auth gate. Everything except the pairing exchange goes
// through it, and it re-checks the device on every request rather than trusting
// the cookie alone — which is what makes a revoke take effect immediately
// instead of at the cookie's month-long expiry.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.sessionFrom(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthenticated",
				"This device is not paired, or its access was revoked.")
			return
		}
		next(w, r.WithContext(withSession(r.Context(), sess)))
	}
}

func (s *Server) sessionFrom(r *http.Request) (Session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return Session{}, false
	}
	return s.devices.VerifySession(c.Value)
}

func (s *Server) handleAPINotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not-found", "No such endpoint.")
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.cfg.StaticHandler == nil {
		http.NotFound(w, r)
		return
	}
	s.cfg.StaticHandler.ServeHTTP(w, r)
}

// --- response helpers -------------------------------------------------------

// writeJSON emits a success body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits the uniform failure envelope. `error` is a stable
// machine-readable code and part of the contract; `message` is a human sentence
// and explicitly is not.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": code, "message": message})
}
