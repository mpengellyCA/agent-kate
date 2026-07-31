package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"agentkate/internal/agent"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// personaDeps builds handlerDeps over real (empty) collaborators plus a
// registered fakeHarness, for driving the launch paths that are plain
// functions rather than RPCs (resumeThread, forkAgentThread). The server is
// constructed but never served: emitLifecycle's Notify simply reaches no
// connections.
func personaDeps(t *testing.T, sessions *session.Store) (handlerDeps, *fakeHarness) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fake := &fakeHarness{personaApplied: true}
	harnesses := harness.NewRegistry("fake")
	harnesses.Register(fake)
	gitCache := gitstatus.NewCache(log)
	t.Cleanup(func() { _ = gitCache.Close() })
	return handlerDeps{
		srv:       ipc.NewServer(filepath.Join(t.TempDir(), "persona.sock"), log),
		harnesses: harnesses,
		turns:     agent.NewTurnTracker(),
		threads:   newThreadRegistry(),
		gitCache:  gitCache,
		sessions:  sessions,
		log:       log,
	}, fake
}

// personaRepo creates a git repo with one commit — what forkAgentThread needs
// to branch a worktree from.
func personaRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", "-q"},
		{"git", "config", "user.email", "test@agentkate"},
		{"git", "config", "user.name", "Agent Kate Test"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"git", "add", "."}, {"git", "commit", "-q", "-m", "init"}} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return repo
}

var personaProfiles = []harness.AgentProfile{{
	Name: "reviewer", Description: "Reviews code", Prompt: "You review.",
	Tools: []string{"Read"}, Model: "fake-small",
}}

// TestResumeReplaysPersona is the heart of the P3 remediation: a thread's
// persona is a launch-time flag, so a resume that does not re-pass it silently
// hands the human a different agent than the one they stopped. Promote ends in
// resumeThread too, so this covers it.
func TestResumeReplaysPersona(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	wtPath := t.TempDir()
	rec := session.Record{
		ThreadID: "t-resume", SessionID: "s-1", Project: t.TempDir(),
		Worktree: worktree.Worktree{ThreadID: "t-resume", Path: wtPath},
		Backend:  "fake", Model: "fake-small", Created: time.Now(),
		Status:       session.StatusDormant,
		SystemPrompt: "You are the arena's scout.",
		Agents:       personaProfiles,
	}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	resumeThread(d, fake, rec, nil)

	spec := fake.spec()
	if !spec.Resume || spec.SessionID != "s-1" {
		t.Fatalf("not a resume: %+v", spec)
	}
	if spec.SystemPrompt != "You are the arena's scout." {
		t.Errorf("resume dropped the system prompt: %q", spec.SystemPrompt)
	}
	if len(spec.Agents) != 1 || spec.Agents[0].Name != "reviewer" ||
		spec.Agents[0].Model != "fake-small" || len(spec.Agents[0].Tools) != 1 {
		t.Errorf("resume dropped the subagent profiles: %+v", spec.Agents)
	}
}

// TestResumeWithoutPersonaStaysEmpty is the backward-compatibility half: a
// record written before P3 (or by a harness that applied nothing) resumes with
// no persona at all, exactly as it always did.
func TestResumeWithoutPersonaStaysEmpty(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	rec := session.Record{
		ThreadID: "t-old", SessionID: "s-old", Project: t.TempDir(),
		Worktree: worktree.Worktree{ThreadID: "t-old", Path: t.TempDir()},
		Backend:  "fake", Created: time.Now(), Status: session.StatusDormant,
	}
	if err := sessions.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	resumeThread(d, fake, rec, nil)

	if spec := fake.spec(); spec.SystemPrompt != "" || spec.Agents != nil {
		t.Fatalf("pre-P3 record resumed with a persona: %q / %+v",
			spec.SystemPrompt, spec.Agents)
	}
}

// TestForkCarriesPersona: a fork continues the source's conversation, so it
// must continue its persona — and record it, so the fork's own later resumes
// keep working.
func TestForkCarriesPersona(t *testing.T) {
	sessions := testSessions(t)
	d, fake := personaDeps(t, sessions)
	repo := personaRepo(t)
	src := session.Record{
		ThreadID: "t-src", SessionID: "s-src", Project: repo,
		Worktree: worktree.Worktree{ThreadID: "t-src", Path: repo},
		Backend:  "fake", Model: "fake-small", Title: "source",
		Created: time.Now(), Status: session.StatusDormant,
		SystemPrompt: "You are the arena's scout.",
		Agents:       personaProfiles,
	}
	if err := sessions.Put(src); err != nil {
		t.Fatalf("Put: %v", err)
	}

	forkAgentThread(d, fake, src, "t-fork", "", "", "the fork")
	t.Cleanup(func() {
		if wt, ok := d.threads.get("t-fork"); ok {
			_ = worktree.Remove(wt)
		}
	})

	spec := fake.spec()
	if !spec.ForkSession {
		t.Fatalf("not a fork: %+v", spec)
	}
	if spec.SystemPrompt != "You are the arena's scout." || len(spec.Agents) != 1 {
		t.Errorf("fork dropped the persona: %q / %+v", spec.SystemPrompt, spec.Agents)
	}
	forked, ok := sessions.Get("t-fork")
	if !ok {
		t.Fatal("fork record missing")
	}
	if forked.SystemPrompt != "You are the arena's scout." || len(forked.Agents) != 1 {
		t.Errorf("fork record dropped the persona: %q / %+v",
			forked.SystemPrompt, forked.Agents)
	}
}

// TestAppliedPersonaRecordsOnlyWhatLanded pins what reaches the record: the
// harness's verdict, never the request. A harness that applied nothing (kimi's
// shape) must persist nothing, so its resumes keep reporting nothing.
func TestAppliedPersonaRecordsOnlyWhatLanded(t *testing.T) {
	requested := []harness.AgentProfile{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	// Kimi's shape: capability false, every profile refused.
	prompt, agents := appliedPersona("persona", requested, harness.Launched{
		Agents: harness.UnappliedAgents(requested, "nope"),
	})
	if prompt != "" || agents != nil {
		t.Errorf("refused persona persisted: %q / %+v", prompt, agents)
	}

	// Mixed: the middle profile refused, the system prompt applied.
	prompt, agents = appliedPersona("persona", requested, harness.Launched{
		SystemPromptApplied: true,
		Agents: []harness.AppliedAgent{
			{Name: "a", Applied: true},
			{Name: "b", Unapplied: []string{"no description"}},
			{Name: "c", Applied: true},
		},
	})
	if prompt != "persona" {
		t.Errorf("applied system prompt not persisted: %q", prompt)
	}
	if len(agents) != 2 || agents[0].Name != "a" || agents[1].Name != "c" {
		t.Errorf("persisted profiles = %+v, want a and c", agents)
	}

	// Under-reported: an adapter that says nothing about a profile must not
	// have it persisted as applied (unappliedPersona names it at launch).
	if _, agents := appliedPersona("", requested, harness.Launched{
		Agents: []harness.AppliedAgent{{Name: "a", Applied: true}},
	}); len(agents) != 1 || agents[0].Name != "a" {
		t.Errorf("under-reported profiles persisted: %+v", agents)
	}
}

// TestLaunchWorkerRecordsAppliedPersona closes the loop over the real RPC: the
// record a launch writes carries the persona the harness took, so the thread
// resumes as the same agent.
func TestLaunchWorkerRecordsAppliedPersona(t *testing.T) {
	for _, tc := range []struct {
		name        string
		applies     bool
		wantPrompt  string
		wantProfile int
	}{
		{"harness applies the persona", true, "You are the arena's scout.", 1},
		{"harness applies nothing", false, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := testSessions(t)
			if err := sessions.Put(session.Record{
				ThreadID: "t-parent", Project: t.TempDir(), Created: time.Now(),
			}); err != nil {
				t.Fatalf("Put: %v", err)
			}
			client := orchTestCore(t, sessions, agent.NewTurnTracker(),
				&fakeHarness{personaApplied: tc.applies})
			var res struct {
				ThreadID string `json:"threadId"`
			}
			if err := client.Call("agent.launchWorker", map[string]any{
				"parentThreadId": "t-parent", "backend": "fake",
				"prompt": "do the thing", "isolation": "workspace",
				"systemPrompt": "You are the arena's scout.",
				"agents":       personaProfiles,
			}, &res); err != nil {
				t.Fatalf("launchWorker: %v", err)
			}
			rec, ok := sessions.Get(res.ThreadID)
			if !ok {
				t.Fatal("worker record missing")
			}
			if rec.SystemPrompt != tc.wantPrompt || len(rec.Agents) != tc.wantProfile {
				t.Fatalf("record persona = %q / %+v", rec.SystemPrompt, rec.Agents)
			}
			// And it survives the store, which is what a resume reads.
			b, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var back session.Record
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if back.SystemPrompt != tc.wantPrompt || len(back.Agents) != tc.wantProfile {
				t.Fatalf("persona lost in the store: %q / %+v",
					back.SystemPrompt, back.Agents)
			}
		})
	}
}
