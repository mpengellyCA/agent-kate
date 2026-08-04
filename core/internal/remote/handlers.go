package remote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxSummaryBytes caps a permission digest on the wire.
//
// The core deliberately does NOT bound Summary: you must be able to read the
// whole Bash command you are approving on the desktop. A phone body is a
// different constraint — an unbounded string here would let one pathological
// tool call dominate a roster response over mobile data — so it is capped at the
// point it leaves the machine, rune-safely, and never at the point it is made.
const maxSummaryBytes = 512

// maxPlanBytes caps ExitPlanMode's markdown on the wire.
//
// It is a second, independent cut: the core caps what it stores, this caps what
// leaves. 16 KiB is ~13x the desktop's own 1200-character display truncation —
// so the phone sees strictly more of a plan than the desktop does, which is the
// right way round for the surface where reading it is the whole point — and
// still fits inside maxRequestBytes with room for JSON escaping, so a client
// that echoes the plan back in updatedInput cannot be refused by its own cap.
const maxPlanBytes = 16 * 1024

// maxSendTextBytes caps a prompt arriving from a phone. Nobody composes 32 KiB
// on a handset; the cap exists so an authenticated-but-hostile device cannot
// push an unbounded body into an agent's stdin.
const maxSendTextBytes = 32 * 1024

const (
	maxRemoteAttachments     = 4
	maxRemoteAttachmentBytes = 4 * 1024 * 1024
	maxRemoteAttachmentTotal = 6 * 1024 * 1024
	maxRemoteAttachmentName  = 160
)

// Transcript paging caps. The defaults are deliberately low: this is the reply
// most likely to be fetched on mobile data, and M0.4's rule is that non-positive
// means "default", never "unlimited" — there is intentionally no way for a
// remote caller to ask for everything.
const (
	defaultTranscriptLimit = 200
	maxTranscriptLimit     = 1000
	defaultTranscriptBytes = 256 * 1024
	maxTranscriptBytes     = 1024 * 1024
)

// Diff caps. A 40 MB diff streamed to a phone on mobile data is the failure this
// pair of numbers exists to prevent.
const (
	defaultDiffBytes = 128 * 1024
	maxDiffBytes     = 512 * 1024
	defaultDiffLines = 2000
	maxDiffLines     = 10000
)

// Git log caps.
const (
	defaultGitLogLimit = 50
	maxGitLogLimit     = 200
)

// sendModeNow is the one send mode the HTTP surface refuses.
//
// M0.3's `now` preserves the old unconditional write to an agent's stdin, which
// is exactly the mid-turn interleaving plan 18 exists to prevent — and the plan
// says the web UI never sends it and it "remains reachable for the desktop",
// i.e. over the local socket. Accepting it here would make the one dangerous
// mode reachable from the least trusted client, so it is rejected rather than
// forwarded. See the report note: this is a deliberate, declared narrowing of
// the frozen payload doc.
const sendModeNow = "now"

func withSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

// sessionOf returns the verified session. Handlers behind requireSession always
// have one; the zero value is returned for the pairing exchange, which has none.
func sessionOf(ctx context.Context) Session {
	s, _ := ctx.Value(sessionCtxKey{}).(Session)
	return s
}

func (s *Server) requireCapability(w http.ResponseWriter, r *http.Request, cap Capability) bool {
	if s.DeviceAllows(sessionOf(r.Context()).DeviceID, cap) {
		return true
	}
	writeError(w, http.StatusForbidden, "capability-required", "This action is not enabled for this paired device.")
	return false
}

// --- auth -------------------------------------------------------------------

func (s *Server) handleAuthExchange(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.limiter.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate-limited",
			"Too many pairing attempts from this address. Wait a minute and try again.")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	dev, err := s.devices.Verify(body.Token)
	if err != nil {
		// One code for every failure mode — unknown token, revoked device, global
		// kill-switch. Distinguishing them would tell an attacker which guesses
		// were close, and tells a legitimate user nothing they can act on.
		writeError(w, http.StatusUnauthorized, "bad-token",
			"That pairing link is not valid. Ask the desktop for a new one.")
		return
	}
	value, expires, err := s.devices.NewSession(dev.ID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "bad-token", "That pairing link is not valid.")
		return
	}
	s.setSessionCookie(w, value, expires)
	_ = s.audit.Append(AuditEntry{
		Kind: AuditAuth, DeviceID: dev.ID, DeviceName: dev.Name,
		Detail: "paired session established from " + ip,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"device": map[string]any{
			"id":       dev.ID,
			"name":     dev.Name,
			"pairedAt": rfc3339(dev.PairedAt),
		},
	})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r.Context())
	s.clearSessionCookie(w)
	// Drop this browser session's live stream too. A logout that leaves an SSE
	// connection feeding the tab you just logged out of is not a logout.
	//
	// Note the honest limit of a stateless cookie: we cannot invalidate the
	// cookie value itself server-side, only ask the browser to discard it. To
	// truly cut a device off, revoke it — that bumps the device epoch and every
	// outstanding cookie stops verifying.
	s.hub.terminate("logged-out", func(c *sseClient) bool { return c.sessionID == sess.ID })
	_ = s.audit.Append(AuditEntry{
		Kind: AuditLogout, DeviceID: sess.DeviceID, DeviceName: sess.DeviceName,
		Detail: "session ended by the device",
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, value string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:  cookieName,
		Value: value,
		Path:  "/",
		// Secure + HttpOnly keeps the cookie off plaintext hops and out of
		// document.scripts — the latter matters most, because the one XSS this
		// app must survive is in rendered agent output. SameSite=Strict means a
		// link from any other origin arrives unauthenticated, which is a full
		// CSRF defence for a same-origin single-page app.
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
		MaxAge:   int(sessionTTL / time.Second),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
}

// clientIP is the rate limiter's key.
//
// It reads RemoteAddr and deliberately ignores X-Forwarded-For: this server
// binds a LAN interface with no trusted proxy in front of it, so a header an
// attacker controls would let them mint a fresh bucket per request and defeat
// the limiter entirely.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- meta -------------------------------------------------------------------

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r.Context())
	caps := []string{}
	if dev, ok := s.devices.Get(sess.DeviceID); ok {
		for _, cap := range dev.Capabilities {
			caps = append(caps, string(cap))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion":   APIVersion,
		"coreVersion":  s.cfg.CoreVersion,
		"webuiBuild":   s.cfg.WebUIBuild,
		"serverTime":   s.nowRFC3339(),
		"capabilities": caps,
	})
}

// --- roster -----------------------------------------------------------------

type awaitingWire struct {
	RequestID string `json:"requestId"`
	Kind      string `json:"kind"`
	ToolName  string `json:"toolName"`
	Summary   string `json:"summary"`
	Deadline  string `json:"deadline"`
}

type agentWire struct {
	ThreadID           string        `json:"threadId"`
	Title              string        `json:"title"`
	Project            string        `json:"project"`
	Backend            string        `json:"backend"`
	EngineName         string        `json:"engineName"`
	Model              string        `json:"model"`
	Status             string        `json:"status"`
	Busy               bool          `json:"busy"`
	AwaitingPermission *awaitingWire `json:"awaitingPermission"`
	Attention          bool          `json:"attention"`
	LastActivityAt     string        `json:"lastActivityAt"`
	ParentThreadID     string        `json:"parentThreadId"`
	Role               string        `json:"role"`
}

func wireAwaiting(a *Awaiting) *awaitingWire {
	if a == nil {
		return nil
	}
	return &awaitingWire{
		RequestID: a.RequestID,
		Kind:      a.Kind,
		ToolName:  a.ToolName,
		Summary:   clip(a.Summary, maxSummaryBytes),
		Deadline:  rfc3339(a.Deadline),
	}
}

func wireAgent(a Agent) agentWire {
	return agentWire{
		ThreadID:           a.ThreadID,
		Title:              a.Title,
		Project:            displayProject(a.Project),
		Backend:            a.Backend,
		EngineName:         a.EngineName,
		Model:              a.Model,
		Status:             a.Status,
		Busy:               a.Busy,
		AwaitingPermission: wireAwaiting(a.AwaitingPermission),
		Attention:          a.Attention,
		LastActivityAt:     rfc3339(a.LastActivityAt),
		ParentThreadID:     a.ParentThreadID,
		Role:               a.Role,
	}
}

// displayProject enforces "never an absolute path" at the wire, not at the
// source. The roster's project field is a display name today, but a rule that
// only holds because of what a caller happens to pass is not a rule.
func displayProject(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *Server) rosterBody(agents []Agent) map[string]any {
	rows := make([]agentWire, 0, len(agents))
	for _, a := range agents {
		rows = append(rows, wireAgent(a))
	}
	return map[string]any{"serverTime": s.nowRFC3339(), "agents": rows}
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	agents, err := s.backend.ListAgents(ctx)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.rosterBody(agents))
}

// handleAgent answers the detail view by filtering the roster rather than
// through a dedicated Backend method. One source for a row means the detail view
// and the roster can never disagree about whether an agent needs you, and it is
// one fewer method for the core's adapter to implement and keep in step.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadId")
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	agents, err := s.backend.ListAgents(ctx)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	for _, a := range agents {
		if a.ThreadID == threadID {
			writeJSON(w, http.StatusOK, map[string]any{
				"serverTime": s.nowRFC3339(),
				"agent":      wireAgent(a),
			})
			return
		}
	}
	writeBackendError(w, ErrUnknownThread)
}

// --- transcript -------------------------------------------------------------

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadId")
	q := r.URL.Query()

	// The SSE cursor is read BEFORE the snapshot, not after.
	//
	// The contract promises a join with "no window in which events are lost or
	// duplicated"; without holding the publish lock across a backend read — which
	// would block the core's fan-out, the one thing this package may never do —
	// only one of the two is achievable. Reading the cursor first means an event
	// published during the fetch is replayed rather than dropped: the client may
	// see it twice, and a duplicate is recoverable where a hole is not.
	cursor := s.hub.headID()

	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	tr, err := s.backend.Transcript(ctx, TranscriptRequest{
		ThreadID: threadID,
		Limit:    clampInt(q.Get("limit"), defaultTranscriptLimit, maxTranscriptLimit),
		Before:   q.Get("before"),
		MaxBytes: clampInt(q.Get("maxBytes"), defaultTranscriptBytes, maxTranscriptBytes),
	})
	if err != nil {
		writeBackendError(w, err)
		return
	}
	events := tr.Events
	if events == nil {
		events = []TranscriptEvent{}
	}
	body := map[string]any{
		"threadId": threadID,
		"events":   events,
		// truncated is emitted unconditionally, even when false: a field that
		// only appears on the bad path is a field nobody handles (M0.4).
		"truncated":   tr.Truncated,
		"hasMore":     tr.HasMore,
		"nextBefore":  tr.NextBefore,
		"lastEventId": cursor,
		"serverTime":  s.nowRFC3339(),
	}
	writeJSON(w, http.StatusOK, body)
}

// --- mutations --------------------------------------------------------------

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadId")
	sess := sessionOf(r.Context())
	var body struct {
		Text        string       `json:"text"`
		Mode        string       `json:"mode"`
		Attachments []Attachment `json:"attachments"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Text) == "" && len(body.Attachments) == 0 {
		writeError(w, http.StatusBadRequest, "empty-text", "There is nothing to send.")
		return
	}
	if len(body.Text) > maxSendTextBytes || !utf8.ValidString(body.Text) {
		writeError(w, http.StatusRequestEntityTooLarge, "text-too-large",
			"That message is too large to send from a remote device.")
		return
	}
	attachments, err := validateRemoteAttachments(body.Attachments)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid-attachment", err.Error())
		return
	}
	mode := body.Mode
	if mode == "" {
		mode = "queue" // remote default, per the contract
	}
	switch mode {
	case "queue":
	case sendModeNow:
		writeError(w, http.StatusBadRequest, "unsupported-mode",
			"Sending immediately is not available from a remote device; queue the message instead.")
		return
	default:
		writeError(w, http.StatusBadRequest, "unsupported-mode", "Unknown send mode.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	res, err := s.backend.Send(ctx, principalOf(r), SendRequest{
		ThreadID: threadID, Text: body.Text, Attachments: attachments, Mode: mode,
	})
	detail := "mode=" + mode
	if len(attachments) > 0 {
		detail += " attachments=" + strconv.Itoa(len(attachments))
	}
	s.auditAction(sess, AuditSend, threadID, "", detail, err)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "queued": res.Queued, "position": res.Position,
		// Additive within v1: a client that does not know the field simply
		// says "queued", which is still true.
		"resuming": res.Resuming,
	})
}

func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapability(w, r, CapAgentManage) {
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Title) > maxSendTextBytes || !utf8.ValidString(body.Title) {
		writeError(w, http.StatusRequestEntityTooLarge, "title-too-large", "That fork title is too large.")
		return
	}
	threadID := r.PathValue("threadId")
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	result, err := s.backend.Fork(ctx, principalOf(r), ForkRequest{ThreadID: threadID, Title: strings.TrimSpace(body.Title)})
	s.auditAction(sessionOf(r.Context()), AuditFork, threadID, "", "fork="+result.ThreadID, err)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "threadId": result.ThreadID})
}

func (s *Server) handleFileRead(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapability(w, r, CapWorktreeView) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	result, err := s.backend.ReadFile(ctx, FileRequest{ThreadID: r.PathValue("threadId"), Path: r.URL.Query().Get("path")})
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) handleFileWrite(w http.ResponseWriter, r *http.Request) {
	if !s.requireCapability(w, r, CapWorktreeEdit) {
		return
	}
	var body struct{ Path, Text, Revision string }
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Text) > 512*1024 || !utf8.ValidString(body.Text) {
		writeError(w, http.StatusRequestEntityTooLarge, "file-too-large", "Files are limited to 512 KiB of UTF-8 text.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	result, err := s.backend.WriteFile(ctx, principalOf(r), FileWriteRequest{ThreadID: r.PathValue("threadId"), Path: body.Path, Text: body.Text, Revision: body.Revision})
	s.auditAction(sessionOf(r.Context()), AuditFileWrite, r.PathValue("threadId"), "", "path="+body.Path, err)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func validateRemoteAttachments(in []Attachment) ([]Attachment, error) {
	if len(in) > maxRemoteAttachments {
		return nil, fmt.Errorf("send at most %d attachments", maxRemoteAttachments)
	}
	out := make([]Attachment, 0, len(in))
	total := 0
	for _, a := range in {
		a.Name = strings.TrimSpace(a.Name)
		if a.Name == "" || len(a.Name) > maxRemoteAttachmentName || !utf8.ValidString(a.Name) ||
			a.Name == "." || a.Name == ".." || strings.ContainsAny(a.Name, "/\\\x00") {
			return nil, fmt.Errorf("attachment name is invalid")
		}
		switch a.Kind {
		case "text":
			if a.MediaType != "text/plain" && a.MediaType != "text/markdown" {
				return nil, fmt.Errorf("text attachment %q has an unsupported type", a.Name)
			}
			if a.DataB64 != "" || !utf8.ValidString(a.Text) || len(a.Text) > maxRemoteAttachmentBytes {
				return nil, fmt.Errorf("text attachment %q is too large or invalid", a.Name)
			}
			total += len(a.Text)
		case "image":
			switch a.MediaType {
			case "image/png", "image/jpeg", "image/gif", "image/webp":
			default:
				return nil, fmt.Errorf("image attachment %q has an unsupported type", a.Name)
			}
			if a.Text != "" || len(a.DataB64) == 0 || len(a.DataB64) > base64.StdEncoding.EncodedLen(maxRemoteAttachmentBytes) {
				return nil, fmt.Errorf("image attachment %q is too large or invalid", a.Name)
			}
			decoded, err := base64.StdEncoding.DecodeString(a.DataB64)
			if err != nil || len(decoded) > maxRemoteAttachmentBytes {
				return nil, fmt.Errorf("image attachment %q is too large or invalid", a.Name)
			}
			total += len(decoded)
		default:
			return nil, fmt.Errorf("attachment %q has an unsupported kind", a.Name)
		}
		if total > maxRemoteAttachmentTotal {
			return nil, fmt.Errorf("attachments exceed the %d MiB limit", maxRemoteAttachmentTotal/(1024*1024))
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	s.simpleAction(w, r, AuditInterrupt, s.backend.Interrupt)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.simpleAction(w, r, AuditStop, s.backend.Stop)
}

func (s *Server) simpleAction(w http.ResponseWriter, r *http.Request, kind AuditKind,
	fn func(context.Context, Principal, string) error) {
	threadID := r.PathValue("threadId")
	sess := sessionOf(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	err := fn(ctx, principalOf(r), threadID)
	s.auditAction(sess, kind, threadID, "", "", err)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// Permission kinds. They are spelled once here because the plan/question
// carve-out below is gated on them, and a typo in that gate is a redaction
// hole rather than a rendering glitch.
const (
	permKindTool     = "tool"
	permKindQuestion = "question"
	permKindPlan     = "plan"
)

// handlePermissionDetail is GET /api/v1/permissions/{requestId} — the renderable
// content of a parked prompt.
//
// The contract's event deliberately carries no `input`, which left a phone able
// to see that a plan needed approving and unable to read it. This route closes
// that hole WITHOUT widening the event: it hands back exactly two fields, both
// model-generated content the human must read in order to answer at all, and
// only for the two kinds that have them.
//
// The kind gate is applied here as well as at the point the request is opened.
// That is deliberate belt-and-braces on the one property this route must not
// get wrong: a `tool` prompt — the Bash command line the redaction rules exist
// for — leaves this handler with a summary and nothing else, even if a future
// backend were careless about what it filled in.
func (s *Server) handlePermissionDetail(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("requestId")
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	d, err := s.backend.PermissionDetail(ctx, requestID)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	body := map[string]any{
		"requestId":  d.RequestID,
		"threadId":   d.ThreadID,
		"kind":       d.Kind,
		"toolName":   d.ToolName,
		"summary":    clip(d.Summary, maxSummaryBytes),
		"deadline":   rfc3339(d.Deadline),
		"serverTime": s.nowRFC3339(),
	}
	switch d.Kind {
	case permKindPlan:
		body["plan"] = clip(d.Plan, maxPlanBytes)
	case permKindQuestion:
		// Emitted even when empty, so a client renders "answer this on the
		// desktop" rather than waiting for a field that is never coming. A
		// non-array is treated as absent: it goes straight into a form builder.
		if isJSONArray(d.Questions) {
			body["questions"] = d.Questions
		} else {
			body["questions"] = []any{}
		}
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handlePermission(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("requestId")
	sess := sessionOf(r.Context())
	var body struct {
		Allow        bool            `json:"allow"`
		DenyMessage  string          `json:"denyMessage"`
		UpdatedInput json.RawMessage `json:"updatedInput"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	// updatedInput goes straight into a harness's stdin as the tool's arguments,
	// so it is validated as a JSON OBJECT here rather than trusted to be one. A
	// scalar or an array would be silently wrong three layers down.
	if len(body.UpdatedInput) > 0 && !isJSONObject(body.UpdatedInput) {
		writeError(w, http.StatusBadRequest, "bad-updated-input",
			"updatedInput must be a JSON object.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	err := s.backend.RespondPermission(ctx, principalOf(r), PermissionAnswer{
		RequestID:    requestID,
		Allow:        body.Allow,
		DenyMessage:  clip(body.DenyMessage, maxSummaryBytes),
		UpdatedInput: body.UpdatedInput,
	})
	s.auditAction(sess, AuditPermission, "", requestID,
		"allow="+strconv.FormatBool(body.Allow), err)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- read-only worktree surfaces --------------------------------------------

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadId")
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	d, err := s.backend.Diff(ctx, DiffRequest{
		ThreadID: threadID,
		MaxBytes: clampInt(q.Get("maxBytes"), defaultDiffBytes, maxDiffBytes),
		MaxLines: clampInt(q.Get("maxLines"), defaultDiffLines, maxDiffLines),
	})
	if err != nil {
		writeBackendError(w, err)
		return
	}
	files := d.Files
	if files == nil {
		// Derived from the patch so an adapter can be a straight passthrough of
		// the core's agent.diff, which returns a patch and no file list.
		files = diffStat(d.Patch)
	}
	rows := make([]map[string]any, 0, len(files))
	for _, f := range files {
		rows = append(rows, map[string]any{
			"path": f.Path, "status": f.Status,
			"additions": f.Additions, "deletions": f.Deletions,
		})
	}
	body := map[string]any{
		"files":     rows,
		"patch":     d.Patch,
		"truncated": d.Truncated,
	}
	if d.Truncated {
		// Present only when truncated, per the contract.
		body["omittedFiles"] = d.OmittedFiles
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	res, err := s.backend.GitStatus(ctx, r.PathValue("threadId"))
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGitLog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), backendTimeout)
	defer cancel()
	limit := clampInt(r.URL.Query().Get("limit"), defaultGitLogLimit, maxGitLogLimit)
	res, err := s.backend.GitLog(ctx, r.PathValue("threadId"), limit)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// --- shared plumbing --------------------------------------------------------

// auditAction records a MUTATING remote action. Read-only routes are
// deliberately not audited: an audit log nobody can read through is one nobody
// reads, and "who approved what, from which device, when" is the question this
// exists to answer.
func (s *Server) auditAction(sess Session, kind AuditKind, threadID, requestID, detail string, err error) {
	e := AuditEntry{
		Kind: kind, DeviceID: sess.DeviceID, DeviceName: sess.DeviceName,
		ThreadID: threadID, RequestID: requestID, Detail: detail,
	}
	if err != nil {
		e.Outcome = "error: " + err.Error()
	} else {
		e.Outcome = "ok"
	}
	_ = s.audit.Append(e)
}

// writeBackendError maps a Backend sentinel onto the contract's status + code
// pair. "You were too late" and "that never existed" are different messages to
// someone staring at a stale lock-screen button, which is why 409 and 404 are
// distinct here rather than collapsed into the broker's bare ok:false.
func writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnknownThread):
		writeError(w, http.StatusNotFound, "unknown-thread", "That agent no longer exists.")
	case errors.Is(err, ErrUnknownRequest):
		writeError(w, http.StatusNotFound, "unknown-request", "That prompt is not one this core knows about.")
	case errors.Is(err, ErrAlreadyResolved):
		writeError(w, http.StatusConflict, "already-resolved", "That prompt was already answered.")
	case errors.Is(err, ErrExpired):
		writeError(w, http.StatusGone, "expired", "That prompt timed out before it was answered.")
	case errors.Is(err, ErrBusy):
		writeError(w, http.StatusConflict, "busy", "That agent is mid-turn.")
	case errors.Is(err, ErrUnsupported):
		writeError(w, http.StatusNotImplemented, "unsupported", "That agent's harness cannot do this.")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "The core did not answer in time.")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "The core could not complete that request.")
	}
}

// decodeJSON reads a request body, rejecting unknown fields so a typo in a
// client is a visible 400 rather than a silently ignored option.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", "The request body could not be read.")
		return false
	}
	return true
}

func isJSONObject(raw json.RawMessage) bool {
	t := strings.TrimLeft(string(raw), " \t\r\n")
	return strings.HasPrefix(t, "{")
}

func isJSONArray(raw json.RawMessage) bool {
	t := strings.TrimLeft(string(raw), " \t\r\n")
	return strings.HasPrefix(t, "[")
}

// clampInt parses a query cap. Non-positive and unparsable both mean "default",
// never "unlimited": there is deliberately no spelling of "send me everything",
// and maxBytes=-1 is exactly what a caller trying to opt out would write.
func clampInt(raw string, def, max int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// clip truncates on a RUNE boundary. A summary that ends in half a character
// renders as a replacement glyph on a phone and looks like corruption, which is
// an alarming thing for a security prompt to look like.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// diffStat derives a per-file stat from a unified diff. Best-effort by design:
// the file list is a summary for a phone's header row, and a patch that has been
// truncated mid-hunk should still produce a usable list rather than an error.
func diffStat(patch string) []DiffFile {
	var out []DiffFile
	var cur *DiffFile
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &DiffFile{Path: gitDiffPath(line), Status: "M"}
		case cur == nil:
			// Preamble before the first file header; nothing to attribute it to.
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = "A"
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "D"
		case strings.HasPrefix(line, "rename to "):
			cur.Status = "R"
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			// File headers, not content.
		case strings.HasPrefix(line, "+"):
			cur.Additions++
		case strings.HasPrefix(line, "-"):
			cur.Deletions++
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func gitDiffPath(line string) string {
	rest := strings.TrimPrefix(line, "diff --git ")
	// Last " b/" rather than first, so a path containing " b/" still resolves.
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return rest
}
