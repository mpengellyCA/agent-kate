package remote

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func sampleAgents() []Agent {
	deadline := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	return []Agent{
		{
			ThreadID: "t-1", Title: "fix the send queue",
			Project: "/home/mike/Dev/AgentKate", Backend: "claude",
			EngineName: "Claude Code", Model: "claude-opus-5", Status: "running",
			Busy: true,
			AwaitingPermission: &Awaiting{
				RequestID: "perm-abc", Kind: "tool", ToolName: "Bash",
				Summary: "Run: npm ci", Deadline: deadline,
			},
			Attention: true, LastActivityAt: deadline, Role: "worker",
			ParentThreadID: "t-0",
		},
		{ThreadID: "t-2", Title: "idle one", Project: "AgentKate", Status: "dormant"},
	}
}

func TestRosterShape(t *testing.T) {
	be := &fakeBackend{agents: sampleAgents()}
	env := newTestEnvWith(t, be)

	code, body := env.authJSON("GET", "/api/v1/agents", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["serverTime"] == nil {
		t.Error("roster carries no serverTime, so a client cannot derive its clock offset")
	}
	rows, _ := body["agents"].([]any)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	first, _ := rows[0].(map[string]any)

	// Every field of the frozen contract, present and named exactly.
	for _, key := range []string{
		"threadId", "title", "project", "backend", "engineName", "model", "status",
		"busy", "awaitingPermission", "attention", "lastActivityAt",
		"parentThreadId", "role",
	} {
		if _, ok := first[key]; !ok {
			t.Errorf("roster row is missing %q", key)
		}
	}
	// The volatile agentId must never appear anywhere.
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "agentId") {
		t.Error("a response mentioned agentId; everything phone-facing keys on threadId")
	}
	// A path must never leave the machine, even through a display field.
	if first["project"] != "AgentKate" {
		t.Errorf("project = %v; an absolute path reached the wire", first["project"])
	}
	aw, _ := first["awaitingPermission"].(map[string]any)
	if aw["summary"] != "Run: npm ci" {
		t.Errorf("awaitingPermission.summary = %v", aw["summary"])
	}
	if aw["deadline"] != "2026-07-31T12:00:00Z" {
		t.Errorf("deadline = %v; deadlines are absolute RFC 3339 UTC", aw["deadline"])
	}
	// awaitingPermission must be explicitly null, not absent, when nothing is
	// parked — a client diffing rows needs to see it go away.
	second, _ := rows[1].(map[string]any)
	v, present := second["awaitingPermission"]
	if !present || v != nil {
		t.Errorf("idle row awaitingPermission = %v (present=%v), want explicit null", v, present)
	}
}

func TestAgentDetailAndUnknownThread(t *testing.T) {
	env := newTestEnvWith(t, &fakeBackend{agents: sampleAgents()})

	code, body := env.authJSON("GET", "/api/v1/agents/t-1", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	agent, _ := body["agent"].(map[string]any)
	if agent["threadId"] != "t-1" {
		t.Errorf("detail returned %v", agent["threadId"])
	}

	code, body = env.authJSON("GET", "/api/v1/agents/t-missing", "")
	if code != http.StatusNotFound {
		t.Fatalf("unknown thread status = %d, want 404", code)
	}
	if body["error"] != "unknown-thread" || body["ok"] != false {
		t.Errorf("error envelope = %v", body)
	}
}

func TestTranscriptCarriesTheSSECursor(t *testing.T) {
	be := &fakeBackend{transcript: Transcript{
		Events:     []TranscriptEvent{{Kind: "assistant", Text: "safe assistant text"}},
		HasMore:    true,
		NextBefore: "cursor-1",
	}}
	env := newTestEnvWith(t, be)
	env.srv.hub.publish(evTurnState, "t-1", []byte(`{}`))
	env.srv.hub.publish(evTurnState, "t-1", []byte(`{}`))

	code, body := env.authJSON("GET", "/api/v1/agents/t-1/transcript?limit=10", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["lastEventId"] != float64(2) {
		t.Errorf("lastEventId = %v, want the hub head; the join would not be gapless",
			body["lastEventId"])
	}
	// truncated is emitted even when false.
	if v, ok := body["truncated"]; !ok || v != false {
		t.Errorf("truncated = %v (present=%v), want an explicit false", v, ok)
	}
	if body["hasMore"] != true || body["nextBefore"] != "cursor-1" {
		t.Errorf("paging fields = %v / %v", body["hasMore"], body["nextBefore"])
	}
}

func TestRemoteSendUsesTypedQueueContract(t *testing.T) {
	backend := &fakeBackend{sendResult: SendResult{Queued: true, Position: 2}}
	env := newTestEnvWith(t, backend)
	code, body := env.authJSON("POST", "/api/v1/agents/t-1/send", `{
        "text":"continue with the safe path",
        "mode":"queue",
        "attachments":[{"kind":"text","name":"notes.md","mediaType":"text/markdown","text":"# brief"}]
    }`)
	if code != http.StatusOK || body["ok"] != true || body["queued"] != true || body["position"] != float64(2) {
		t.Fatalf("remote send = %d / %#v", code, body)
	}
	backend.mu.Lock()
	got := backend.lastSend
	backend.mu.Unlock()
	if got.ThreadID != "t-1" || got.Mode != "queue" || got.Text != "continue with the safe path" {
		t.Fatalf("send request = %#v", got)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].Name != "notes.md" || got.Attachments[0].Text != "# brief" {
		t.Fatalf("attachments = %#v", got.Attachments)
	}
}

func TestRemoteForkNeedsExplicitDeveloperCapability(t *testing.T) {
	env := newTestEnv(t)
	code, body := env.authJSON("POST", "/api/v1/agents/t-1/fork", `{"title":"mobile continuation"}`)
	if code != http.StatusForbidden || body["error"] != "capability-required" {
		t.Fatalf("ungranted fork = %d / %#v", code, body)
	}
	if _, changed, err := env.srv.SetDeviceCapabilities(env.device.ID, []Capability{CapAgentManage}); err != nil || !changed {
		t.Fatalf("grant = changed=%v err=%v", changed, err)
	}
	code, body = env.authJSON("POST", "/api/v1/agents/t-1/fork", `{"title":"mobile continuation"}`)
	if code != http.StatusAccepted || body["threadId"] != "fork-t-1" {
		t.Fatalf("granted fork = %d / %#v", code, body)
	}
}

func TestRemoteSendRejectsUnsafeAttachmentAndImmediateMode(t *testing.T) {
	for name, body := range map[string]string{
		"path":      `{"text":"x","attachments":[{"kind":"text","name":"../secret","mediaType":"text/plain","text":"x"}]}`,
		"raw image": `{"text":"x","attachments":[{"kind":"image","name":"x.png","mediaType":"image/png","text":"not allowed"}]}`,
		"immediate": `{"text":"x","mode":"now"}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t)
			code, got := env.authJSON("POST", "/api/v1/agents/t-1/send", body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d / %#v, want 400", code, got)
			}
		})
	}
}

func TestValidateRemoteAttachmentsKeepsTheUploadDTONarrow(t *testing.T) {
	valid, err := validateRemoteAttachments([]Attachment{
		{Kind: "text", Name: "notes.md", MediaType: "text/markdown", Text: "# safe"},
		{Kind: "image", Name: "photo.png", MediaType: "image/png", DataB64: "cG5n"},
	})
	if err != nil || len(valid) != 2 {
		t.Fatalf("valid attachments = %#v, %v", valid, err)
	}
	for _, attachment := range []Attachment{
		{Kind: "text", Name: "/tmp/x", MediaType: "text/plain", Text: "x"},
		{Kind: "text", Name: "x.txt", MediaType: "application/json", Text: "{}"},
		{Kind: "image", Name: "x.png", MediaType: "image/png", DataB64: "not-base64"},
		{Kind: "image", Name: "x.svg", MediaType: "image/svg+xml", DataB64: "c3Zn"},
	} {
		if _, err := validateRemoteAttachments([]Attachment{attachment}); err == nil {
			t.Fatalf("unsafe attachment was accepted: %#v", attachment)
		}
	}
}

func TestPermissionAnswerErrorMapping(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		err      error
		wantCode int
		wantErr  string
	}{
		{name: "allow", body: `{"allow":true}`, wantCode: 200},
		{name: "deny with a reason", body: `{"allow":false,"denyMessage":"not on main"}`, wantCode: 200},
		{
			name: "question answers ride in updatedInput",
			body: `{"allow":true,"updatedInput":{"answers":{"Which?":"A"}}}`, wantCode: 200,
		},
		{
			name: "already resolved", body: `{"allow":true}`, err: ErrAlreadyResolved,
			wantCode: 409, wantErr: "already-resolved",
		},
		{
			name: "unknown request", body: `{"allow":true}`, err: ErrUnknownRequest,
			wantCode: 404, wantErr: "unknown-request",
		},
		{
			name: "expired", body: `{"allow":true}`, err: ErrExpired,
			wantCode: 410, wantErr: "expired",
		},
		{
			name: "updatedInput must be an object", body: `{"allow":true,"updatedInput":[1,2]}`,
			wantCode: 400, wantErr: "bad-updated-input",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnvWith(t, &fakeBackend{permErr: tc.err})
			code, body := env.authJSON("POST", "/api/v1/permissions/perm-abc", tc.body)
			if code != tc.wantCode {
				t.Fatalf("status = %d, want %d (%v)", code, tc.wantCode, body)
			}
			if tc.wantErr != "" && body["error"] != tc.wantErr {
				t.Errorf("error = %v, want %q", body["error"], tc.wantErr)
			}
			if tc.wantCode == 200 && body["ok"] != true {
				t.Errorf("ok = %v", body["ok"])
			}
		})
	}
}

// TestPermissionDetailIsKindGated is the amendment's entire security argument,
// as a test. A plan and a question hand back the content you need to answer
// them; a `tool` prompt hands back the summary and NOTHING else — no plan, no
// questions, and no `input` under any spelling.
//
// The tool case deliberately arrives from a backend that filled Plan and
// Questions anyway. The gate that matters is the one in the handler, because it
// is the one that still holds when a future backend gets it wrong.
func TestPermissionDetailIsKindGated(t *testing.T) {
	const secret = "deploy --token=hunter2"
	deadline := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		detail PermissionDetail
		want   map[string]any // fields that must be present, with their values
		absent []string
	}{
		{
			name: "plan carries its markdown",
			detail: PermissionDetail{
				RequestID: "perm-1", ThreadID: "t-1", Kind: permKindPlan,
				ToolName: "ExitPlanMode", Summary: "finish planning",
				Deadline: deadline, Plan: "# The plan\n\nrewire the relay",
			},
			want:   map[string]any{"plan": "# The plan\n\nrewire the relay"},
			absent: []string{"questions", "input"},
		},
		{
			name: "question carries its list, verbatim",
			detail: PermissionDetail{
				RequestID: "perm-2", ThreadID: "t-1", Kind: permKindQuestion,
				ToolName: "AskUserQuestion", Deadline: deadline,
				Questions: json.RawMessage(
					`[{"question":"Which?","options":["A","B"],"multiSelect":false}]`),
			},
			absent: []string{"plan", "input"},
		},
		{
			name: "a tool prompt gets the summary and nothing else",
			detail: PermissionDetail{
				RequestID: "perm-3", ThreadID: "t-1", Kind: permKindTool,
				ToolName: "Bash", Summary: "Run: " + secret, Deadline: deadline,
				// A backend that filled these in regardless. The handler must
				// still withhold them.
				Plan:      "# a plan that is not this prompt's",
				Questions: json.RawMessage(`[{"question":"leak me"}]`),
			},
			want:   map[string]any{"summary": "Run: " + secret},
			absent: []string{"plan", "questions", "input"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnvWith(t, &fakeBackend{permDetail: tc.detail})
			code, body := env.authJSON("GET", "/api/v1/permissions/"+tc.detail.RequestID, "")
			if code != http.StatusOK {
				t.Fatalf("status = %d (%v)", code, body)
			}
			for k, v := range tc.want {
				if body[k] != v {
					t.Errorf("%s = %v, want %v", k, body[k], v)
				}
			}
			for _, k := range tc.absent {
				if _, has := body[k]; has {
					t.Errorf("kind %q leaked %q: %v", tc.detail.Kind, k, body[k])
				}
			}
			if body["kind"] != tc.detail.Kind || body["toolName"] != tc.detail.ToolName {
				t.Errorf("kind/toolName = %v/%v", body["kind"], body["toolName"])
			}
			if body["deadline"] != "2026-07-31T12:00:00Z" {
				t.Errorf("deadline = %v", body["deadline"])
			}
			if _, err := time.Parse(time.RFC3339, fmt.Sprint(body["serverTime"])); err != nil {
				t.Errorf("serverTime %v is not RFC3339", body["serverTime"])
			}
			if tc.detail.Kind == permKindQuestion {
				qs, ok := body["questions"].([]any)
				if !ok || len(qs) != 1 {
					t.Fatalf("questions = %v", body["questions"])
				}
				q, _ := qs[0].(map[string]any)
				if q["question"] != "Which?" {
					t.Errorf("question text was not echoed verbatim: %v", q)
				}
			}
		})
	}
}

// TestPermissionDetailPlanIsCapped: a plan is model-generated and unbounded, and
// this reply is fetched over mobile data.
func TestPermissionDetailPlanIsCapped(t *testing.T) {
	env := newTestEnvWith(t, &fakeBackend{permDetail: PermissionDetail{
		RequestID: "perm-1", Kind: permKindPlan, ToolName: "ExitPlanMode",
		Plan: strings.Repeat("ünïcödé ", 8000),
	}})
	_, body := env.authJSON("GET", "/api/v1/permissions/perm-1", "")
	plan, _ := body["plan"].(string)
	if len(plan) > maxPlanBytes+4 {
		t.Errorf("plan not capped: %d bytes", len(plan))
	}
	if !strings.HasSuffix(plan, "…") {
		t.Error("a capped plan should be marked elided")
	}
}

// TestPermissionDetailUnknownRequestIs404: "already answered" and "never
// existed" look the same from here, and both mean there is nothing to render.
func TestPermissionDetailUnknownRequestIs404(t *testing.T) {
	env := newTestEnvWith(t, &fakeBackend{permDetailErr: ErrUnknownRequest})
	code, body := env.authJSON("GET", "/api/v1/permissions/perm-gone", "")
	if code != http.StatusNotFound || body["error"] != "unknown-request" {
		t.Fatalf("status = %d, error = %v", code, body["error"])
	}
}

func TestDiffDerivesFileStatsFromThePatch(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/core/x.go b/core/x.go",
		"index 111..222 100644",
		"--- a/core/x.go",
		"+++ b/core/x.go",
		"@@ -1,3 +1,4 @@",
		" ctx",
		"+added one",
		"+added two",
		"-removed one",
		"diff --git a/new.txt b/new.txt",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/new.txt",
		"+hello",
	}, "\n")
	env := newTestEnvWith(t, &fakeBackend{diff: Diff{Patch: patch, Truncated: true, OmittedFiles: 4}})

	code, body := env.authJSON("GET", "/api/v1/agents/t-1/diff", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	files, _ := body["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("derived %d files, want 2 (%v)", len(files), files)
	}
	first, _ := files[0].(map[string]any)
	if first["path"] != "core/x.go" || first["status"] != "M" ||
		first["additions"] != float64(2) || first["deletions"] != float64(1) {
		t.Errorf("first file = %v", first)
	}
	second, _ := files[1].(map[string]any)
	if second["path"] != "new.txt" || second["status"] != "A" {
		t.Errorf("second file = %v", second)
	}
	if body["truncated"] != true || body["omittedFiles"] != float64(4) {
		t.Errorf("truncation fields = %v / %v", body["truncated"], body["omittedFiles"])
	}
}

func TestDiffOmitsOmittedFilesWhenNotTruncated(t *testing.T) {
	env := newTestEnvWith(t, &fakeBackend{diff: Diff{Patch: ""}})
	_, body := env.authJSON("GET", "/api/v1/agents/t-1/diff", "")
	if _, present := body["omittedFiles"]; present {
		t.Error("omittedFiles is present on an untruncated diff; the contract says otherwise")
	}
	if body["truncated"] != false {
		t.Errorf("truncated = %v, want an explicit false", body["truncated"])
	}
}

func TestMetaShape(t *testing.T) {
	env := newTestEnv(t)
	code, body := env.authJSON("GET", "/api/v1/meta", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["apiVersion"] != float64(APIVersion) {
		t.Errorf("apiVersion = %v", body["apiVersion"])
	}
	if body["coreVersion"] != "0.test" || body["webuiBuild"] != "test-build" {
		t.Errorf("version fields = %v / %v", body["coreVersion"], body["webuiBuild"])
	}
	if body["serverTime"] == nil {
		t.Error("meta carries no serverTime")
	}
}

func TestGitRoutesPassThrough(t *testing.T) {
	be := &fakeBackend{
		gitStatus: map[string]any{"branch": "agentkate/t-1", "dirty": true},
		gitLog:    map[string]any{"commits": []any{}},
	}
	env := newTestEnvWith(t, be)

	if code, body := env.authJSON("GET", "/api/v1/agents/t-1/git/status", ""); code != 200 ||
		body["branch"] != "agentkate/t-1" {
		t.Errorf("git status = %d %v", code, body)
	}
	if code, body := env.authJSON("GET", "/api/v1/agents/t-1/git/log?limit=5", ""); code != 200 ||
		body["commits"] == nil {
		t.Errorf("git log = %d %v", code, body)
	}
}

func TestInterruptAndStop(t *testing.T) {
	be := &fakeBackend{}
	env := newTestEnvWith(t, be)
	if code, body := env.authJSON("POST", "/api/v1/agents/t-1/interrupt", ""); code != 200 ||
		body["ok"] != true {
		t.Errorf("interrupt = %d %v", code, body)
	}
	if code, body := env.authJSON("POST", "/api/v1/agents/t-1/stop", ""); code != 200 ||
		body["ok"] != true {
		t.Errorf("stop = %d %v", code, body)
	}
	be.mu.Lock()
	be.stopErr = ErrUnknownThread
	be.mu.Unlock()
	if code, body := env.authJSON("POST", "/api/v1/agents/t-1/stop", ""); code != 404 ||
		body["error"] != "unknown-thread" {
		t.Errorf("stop on a missing thread = %d %v", code, body)
	}
}

// TestMutatingActionsAreAudited pins B5's groundwork: mutations are recorded,
// reads are not. An audit log that also holds every roster poll is one nobody
// reads.
func TestMutatingActionsAreAudited(t *testing.T) {
	env := newTestEnvWith(t, &fakeBackend{agents: sampleAgents()})

	env.authJSON("GET", "/api/v1/agents", "")
	env.authJSON("GET", "/api/v1/agents/t-1/diff", "")
	env.authJSON("POST", "/api/v1/permissions/perm-abc", `{"allow":true}`)
	env.authJSON("POST", "/api/v1/agents/t-1/stop", "")

	entries, _, err := env.srv.AuditTail(0, 100)
	if err != nil {
		t.Fatalf("AuditTail: %v", err)
	}
	var kinds []AuditKind
	for _, e := range entries {
		kinds = append(kinds, e.Kind)
		if e.DeviceName == "" && e.Kind != AuditKill && e.Kind != AuditRearm {
			t.Errorf("entry %v has no device attribution", e.Kind)
		}
	}
	want := []AuditKind{AuditPair, AuditAuth, AuditPermission, AuditStop}
	if len(kinds) != len(want) {
		t.Fatalf("audit kinds = %v, want %v (reads must not be audited)", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("audit entry %d = %v, want %v", i, kinds[i], want[i])
		}
	}
}

func TestLogoutClearsTheCookieAndDropsTheStream(t *testing.T) {
	env := newTestEnv(t)
	c := env.openSSE("?scope=roster", -1)
	c.skipRetry()
	c.next() // hello

	resp := env.auth("POST", "/api/v1/auth/logout", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	cleared := false
	for _, ck := range resp.Cookies() {
		if ck.Name == cookieName && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("logout did not clear the session cookie")
	}
	f := c.next()
	if f.event != evRevoked {
		t.Fatalf("stream frame after logout = %q, want revoked", f.event)
	}
	c.expectClosed()
}
