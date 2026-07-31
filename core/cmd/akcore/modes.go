package main

import (
	"context"
	"encoding/json"
	"strings"

	"agentkate/internal/agent"
	"agentkate/internal/harness"
	"agentkate/internal/ipc"
	"agentkate/internal/modes"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

// permissiveModes maps each registered backend to the permission mode that
// runs a worker unattended, taken from that harness's own declared vocabulary
// — never from a hardcoded table. It feeds the master prompt's roster hint, so
// a controller can tell its crew to stop asking the human without the ensemble
// author having to know each engine's spelling.
//
// The rule is positional, not name-matched: a harness lists its modes from the
// most supervised to the least (claude: acceptEdits → default → plan → auto →
// bypassPermissions), so the last entry is the permissive end. A harness with a
// discovered (empty) vocabulary contributes nothing, and its roles get no hint
// rather than a guessed one.
func permissiveModes(reg *harness.Registry) map[string]string {
	out := map[string]string{}
	for _, h := range reg.All() {
		caps := h.Capabilities()
		if n := len(caps.PermissionModes); n > 0 {
			out[caps.ID] = caps.PermissionModes[n-1]
		}
	}
	return out
}

// modeToJSON is the wire shape of one ensemble. Kept identical to the store's
// JSON tags so the UI editor round-trips a mode.get straight back into a
// mode.save without a translation layer.
func modeToJSON(m modes.Mode) map[string]any {
	workers := make([]map[string]any, 0, len(m.Workers))
	for _, w := range m.Workers {
		workers = append(workers, map[string]any{
			"role":           w.Role,
			"backend":        w.Backend,
			"model":          w.Model,
			"permissionMode": w.PermissionMode,
			"effort":         w.Effort,
			"isolation":      w.Isolation,
			"notes":          w.Notes,
		})
	}
	return map[string]any{
		"name":        m.Name,
		"description": m.Description,
		"controller": map[string]any{
			"backend":        m.Controller.Backend,
			"model":          m.Controller.Model,
			"permissionMode": m.Controller.PermissionMode,
			"effort":         m.Controller.Effort,
			"isolation":      m.Controller.Isolation,
		},
		"workers":      workers,
		"masterPrompt": m.MasterPrompt,
		"builtIn":      m.BuiltIn,
	}
}

func registerModeHandlers(d handlerDeps) {
	// mode.list serves the merged catalogue (built-ins + the user's), plus the
	// default master prompt so the editor can show what an empty field means.
	d.srv.Handle("mode.list", func(_ context.Context, _ json.RawMessage) (any, error) {
		list := d.modes.List()
		out := make([]map[string]any, 0, len(list))
		for _, m := range list {
			out = append(out, modeToJSON(m))
		}
		return map[string]any{
			"modes":               out,
			"defaultMasterPrompt": modes.DefaultMasterPrompt,
		}, nil
	})

	d.srv.Handle("mode.get", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		m, ok := d.modes.Get(p.Name)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown ensemble "+p.Name)
		}
		return map[string]any{"mode": modeToJSON(m)}, nil
	})

	// mode.save inserts or replaces one ensemble. Saving under a built-in's
	// name shadows that built-in; the built-in itself is never edited.
	d.srv.Handle("mode.save", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Mode modes.Mode `json:"mode"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		saved, err := d.modes.Save(p.Mode)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		return map[string]any{"mode": modeToJSON(saved)}, nil
	})

	d.srv.Handle("mode.delete", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if _, ok := d.modes.Get(p.Name); !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown ensemble "+p.Name)
		}
		if err := d.modes.Delete(p.Name); err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		return map[string]any{"deleted": p.Name}, nil
	})

	// mode.apply starts the ensemble: ONE thread (the controller), briefed with
	// the rendered master prompt. Workers are deliberately not pre-spawned —
	// the controller launches the roles it actually needs via launch_agent, so
	// applying an ensemble costs one agent, not five worktrees.
	d.srv.Handle("mode.apply", func(_ context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Name          string `json:"name"`
			WorkDir       string `json:"workDir"`
			Task          string `json:"task"`
			CoworkEnabled bool   `json:"coworkEnabled"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
		}
		if strings.TrimSpace(p.WorkDir) == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "workDir is required")
		}
		m, ok := d.modes.Get(p.Name)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown ensemble "+p.Name)
		}
		h, ok := d.harnesses.Get(m.Controller.Backend)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"ensemble "+m.Name+" wants unknown engine "+m.Controller.Backend)
		}
		caps := h.Capabilities()
		switch m.Controller.Isolation {
		case "", worktree.ModeAuto, worktree.ModeIsolated, worktree.ModeWorkspace:
		default:
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"isolation must be auto, isolated or workspace")
		}
		if p.CoworkEnabled && !caps.Cowork {
			return nil, unsupported("Cowork", caps)
		}

		prompt := modes.Render(m, p.WorkDir, permissiveModes(d.harnesses))
		if task := strings.TrimSpace(p.Task); task != "" {
			prompt += "\n\n## The task\n\n" + task
		}
		// The master prompt goes in the OPENING MESSAGE on every harness, so
		// both engines get byte-identical instructions. Where the persona
		// channel exists it ALSO goes there, so the orchestration rules survive
		// the opening message scrolling out of context on a long run — and
		// where it does not, that is reported, never emulated.
		systemPrompt := ""
		if caps.SystemPrompt {
			systemPrompt = prompt
		}

		threadID := agent.NewThreadID()
		sessionID := ""
		if caps.MintsSessionID {
			sessionID = session.NewID()
		}
		// The opening prompt is a turn; queue it before the launch so an
		// agent.wait racing the apply never sees a false idle.
		d.turns.TurnQueued(threadID)
		launched, wt, err := launchThread(d, h, threadID, sessionID, agentStartParams{
			WorkspacePath:  p.WorkDir,
			Prompt:         prompt,
			Backend:        caps.ID,
			Model:          m.Controller.Model,
			Effort:         m.Controller.Effort,
			PermissionMode: m.Controller.PermissionMode,
			Isolation:      m.Controller.Isolation,
			CoworkEnabled:  p.CoworkEnabled,
			SystemPrompt:   systemPrompt,
		}, launchMeta{Title: m.Name, Role: session.RoleController})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
		}
		// Applied-truth, same contract as launch_agent: what the harness took,
		// and what it could not — including the persona channel, so a kimi
		// controller's reply says its master prompt reached it as the opening
		// message only.
		unapplied := unappliedOptions(map[string]string{
			"model":          m.Controller.Model,
			"effort":         m.Controller.Effort,
			"permissionMode": m.Controller.PermissionMode,
		}, launched)
		unapplied = append(unapplied,
			unappliedPersona(systemPrompt, nil, launched, caps)...)
		return map[string]any{
			"threadId":  threadID,
			"sessionId": launched.SessionID,
			"backend":   caps.ID,
			"ensemble":  m.Name,
			"isolated":  wt.Isolated,
			"branch":    wt.Branch,
			"applied": map[string]string{
				"model":          launched.Model,
				"effort":         launched.Effort,
				"permissionMode": launched.PermissionMode,
			},
			"systemPromptApplied": launched.SystemPromptApplied,
			"unapplied":           unapplied,
		}, nil
	})
}
