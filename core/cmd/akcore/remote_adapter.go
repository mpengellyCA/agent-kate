package main

// This file is the narrow seam between the HTTPS transport and the current
// core. It deliberately does not speak IPC: a verified paired device is a
// remote-human principal, never a UI window and never an agent bridge.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"agentkate/internal/gitstatus"
	"agentkate/internal/permission"
	"agentkate/internal/remote"
	"agentkate/internal/worktree"
)

type remoteBackend struct{ d handlerDeps }

var _ remote.Backend = (*remoteBackend)(nil)

func (b *remoteBackend) requireThread(threadID string) error {
	if b.d.sessions == nil {
		return remote.ErrUnknownThread
	}
	if _, ok := b.d.sessions.Get(threadID); !ok {
		return remote.ErrUnknownThread
	}
	return nil
}

func (b *remoteBackend) ListAgents(ctx context.Context) ([]remote.Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.d.sessions == nil || b.d.harnesses == nil {
		return []remote.Agent{}, nil
	}
	busy := map[string]bool{}
	if b.d.turns != nil {
		busy = b.d.turns.Snapshot()
	}
	parked := remotePendingByThread(b.d.broker)
	recs := b.d.sessions.List("")
	rows := make([]remote.Agent, 0, len(recs))
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		descriptor := b.d.descriptorFor(rec.ThreadID)
		awaiting := remoteAwaiting(parked[rec.ThreadID])
		model := rec.EffectiveSettings.Model
		if model == "" {
			// Legacy records are projected in memory from the linkage DTO; the
			// fallback keeps a pre-linkage record readable without branching on
			// any backend identifier.
			model = rec.Model
		}
		last := rec.LastTurnAt
		if last.IsZero() {
			last = rec.Updated
		}
		isBusy := busy[rec.ThreadID]
		rows = append(rows, remote.Agent{
			ThreadID:           rec.ThreadID,
			Title:              remoteClip(rec.Title, remoteTitleBytes),
			Project:            remoteProjectName(rec.Project),
			Backend:            descriptor.ID,
			EngineName:         descriptor.DisplayName,
			Model:              remoteClip(model, remoteTitleBytes),
			Status:             rec.Status,
			Busy:               isBusy,
			AwaitingPermission: awaiting,
			Attention:          awaiting != nil || (rec.Status == "running" && !isBusy),
			LastActivityAt:     last,
			ParentThreadID:     rec.ParentThreadID,
			Role:               rec.Role,
		})
	}
	return rows, nil
}

const remoteTitleBytes = 512

func remoteProjectName(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return ""
	}
	name := filepath.Base(filepath.Clean(project))
	if name == "." || name == string(filepath.Separator) {
		return ""
	}
	return remoteClip(name, remoteTitleBytes)
}

func remotePendingByThread(broker *permission.Broker) map[string]permission.Request {
	parked := make(map[string]permission.Request)
	if broker == nil {
		return parked
	}
	for _, req := range broker.PendingRemote() {
		if req.ThreadID == "" {
			continue
		}
		if _, exists := parked[req.ThreadID]; !exists {
			parked[req.ThreadID] = req
		}
	}
	return parked
}

func remoteAwaiting(req permission.Request) *remote.Awaiting {
	if req.ID == "" {
		return nil
	}
	return &remote.Awaiting{
		RequestID: req.ID,
		Kind:      remotePermissionKind(req.ToolName),
		ToolName:  remoteClip(req.ToolName, remoteTitleBytes),
		Summary:   req.Summary,
		Deadline:  req.Deadline,
	}
}

func remotePermissionKind(toolName string) string {
	switch toolName {
	case "AskUserQuestion":
		return "question"
	case "ExitPlanMode":
		return "plan"
	default:
		return "tool"
	}
}

func (b *remoteBackend) Transcript(ctx context.Context, req remote.TranscriptRequest) (remote.Transcript, error) {
	if err := ctx.Err(); err != nil {
		return remote.Transcript{}, err
	}
	if err := b.requireThread(req.ThreadID); err != nil {
		return remote.Transcript{}, err
	}
	rec, _ := b.d.sessions.Get(req.ThreadID)
	events, err := b.d.harnessFor(req.ThreadID).ReadTranscript(req.ThreadID, rec.AgentRef.NativeSessionID)
	if err != nil {
		return remote.Transcript{}, err
	}
	projected, truncated := projectRemoteTranscript(events, req.Limit, req.MaxBytes)
	return remote.Transcript{Events: projected, Truncated: truncated}, nil
}

// Send remains intentionally unavailable. The route is absent too: until a
// human message has one canonical desktop + remote transcript echo, accepting
// an HTTPS send would create a split conversation history.
func (b *remoteBackend) Send(context.Context, remote.Principal, remote.SendRequest) (remote.SendResult, error) {
	return remote.SendResult{}, remote.ErrUnsupported
}

func (b *remoteBackend) PermissionDetail(ctx context.Context, requestID string) (remote.PermissionDetail, error) {
	if err := ctx.Err(); err != nil {
		return remote.PermissionDetail{}, err
	}
	if b.d.broker == nil {
		return remote.PermissionDetail{}, remote.ErrUnknownRequest
	}
	req, ok := b.d.broker.GetRemote(requestID)
	if !ok {
		return remote.PermissionDetail{}, remote.ErrUnknownRequest
	}
	return remote.PermissionDetail{
		RequestID: req.ID,
		ThreadID:  req.ThreadID,
		Kind:      remotePermissionKind(req.ToolName),
		ToolName:  remoteClip(req.ToolName, remoteTitleBytes),
		Summary:   req.Summary,
		Deadline:  req.Deadline,
		Plan:      req.Plan,
		Questions: append(json.RawMessage(nil), req.Questions...),
	}, nil
}

func (b *remoteBackend) RespondPermission(ctx context.Context, principal remote.Principal, answer remote.PermissionAnswer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.d.broker == nil {
		return remote.ErrUnknownRequest
	}
	if _, ok := b.d.broker.GetRemote(answer.RequestID); !ok {
		return remote.ErrUnknownRequest
	}
	p := humanPrincipal{Surface: remoteSurface, Device: principal.DeviceName}
	if !b.d.humanRespondPermission(p, answer.RequestID, answer.Allow, answer.UpdatedInput) {
		return remote.ErrAlreadyResolved
	}
	return nil
}

func (b *remoteBackend) Interrupt(ctx context.Context, principal remote.Principal, threadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.requireThread(threadID); err != nil {
		return err
	}
	return b.d.humanInterrupt(humanPrincipal{Surface: remoteSurface, Device: principal.DeviceName}, threadID)
}

func (b *remoteBackend) Stop(ctx context.Context, principal remote.Principal, threadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.requireThread(threadID); err != nil {
		return err
	}
	return b.d.humanStop(humanPrincipal{Surface: remoteSurface, Device: principal.DeviceName}, threadID)
}

func (b *remoteBackend) Diff(ctx context.Context, req remote.DiffRequest) (remote.Diff, error) {
	if err := ctx.Err(); err != nil {
		return remote.Diff{}, err
	}
	if err := b.requireThread(req.ThreadID); err != nil {
		return remote.Diff{}, err
	}
	if b.d.threads == nil {
		return remote.Diff{}, remote.ErrUnknownThread
	}
	wt, ok := b.d.threads.get(req.ThreadID)
	if !ok {
		return remote.Diff{}, remote.ErrUnknownThread
	}
	patch, err := worktree.Diff(wt)
	if err != nil {
		return remote.Diff{}, err
	}
	patch, truncated := remoteCapLinesAndBytes(patch, req.MaxLines, req.MaxBytes)
	return remote.Diff{Patch: patch, Truncated: truncated}, nil
}

func (b *remoteBackend) GitStatus(ctx context.Context, threadID string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.requireThread(threadID); err != nil {
		return nil, err
	}
	if b.d.gitCache == nil {
		return nil, remote.ErrUnknownThread
	}
	snap, ok := b.d.gitCache.SnapshotFor(threadID)
	if !ok || snap == nil {
		return nil, remote.ErrUnknownThread
	}
	files := make([]map[string]any, 0, len(snap.Files))
	for _, file := range snap.Files {
		files = append(files, map[string]any{"path": file.Path, "status": file.Status})
	}
	// RepoRoot and Path are deliberately not projected: they are absolute paths
	// on the desktop and a paired browser has no reason to receive either.
	return map[string]any{
		"threadId": threadID, "branch": snap.Branch, "base": snap.Base,
		"headSha": snap.HeadSHA, "isolated": snap.Isolated, "ahead": snap.Ahead,
		"behindBase": snap.BehindBase, "dirtyCount": snap.DirtyCount,
		"hasConflicts": snap.HasConflicts, "hasUpstream": snap.HasUpstream,
		"remoteAhead": snap.RemoteAhead, "remoteBehind": snap.RemoteBehind,
		"files": files, "updatedAt": snap.UpdatedAt.UTC().Format(time.RFC3339),
		"error": snap.Error,
	}, nil
}

func (b *remoteBackend) GitLog(ctx context.Context, threadID string, limit int) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.requireThread(threadID); err != nil {
		return nil, err
	}
	if b.d.threads == nil {
		return nil, remote.ErrUnknownThread
	}
	wt, ok := b.d.threads.get(threadID)
	if !ok {
		return nil, remote.ErrUnknownThread
	}
	if b.d.gitCache != nil {
		if entries, cached, err := b.d.gitCache.LogPageFor(threadID, gitstatus.LogOptions{Limit: limit}); err != nil {
			return nil, err
		} else if cached {
			if entries == nil {
				entries = []gitstatus.LogEntry{}
			}
			return map[string]any{"entries": entries}, nil
		}
	}
	entries, err := gitstatus.Log(wt, gitstatus.LogOptions{Limit: limit})
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []gitstatus.LogEntry{}
	}
	return map[string]any{"entries": entries}, nil
}

type remoteWireEvent struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Phase     string          `json:"phase"`
	Message   json.RawMessage `json:"message"`
}

type remoteWireMessage struct {
	Content json.RawMessage `json:"content"`
}

type remoteWireBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// projectRemoteTranscript is the one raw-event boundary. It only admits user
// prose, assistant prose, a tool name plus a generic summary, and a finite
// lifecycle vocabulary. No tool argument, result payload, diagnostic detail,
// attachment body, or raw permission input has a destination field.
func projectRemoteTranscript(raw []json.RawMessage, limit, maxBytes int) ([]remote.TranscriptEvent, bool) {
	if limit <= 0 {
		limit = 200
	}
	if maxBytes <= 0 {
		maxBytes = 256 * 1024
	}
	result := make([]remote.TranscriptEvent, 0, min(limit, len(raw)))
	remaining := maxBytes
	truncated := false
	appendEvent := func(event remote.TranscriptEvent) bool {
		if len(result) >= limit {
			truncated = true
			return false
		}
		used := len(event.Kind) + len(event.ToolName) + len(event.Summary)
		if event.Text != "" {
			text, clipped := remoteClipToBudget(event.Text, remaining-used)
			event.Text = text
			if clipped {
				truncated = true
			}
		}
		used += len(event.Text)
		if used > remaining {
			truncated = true
			return false
		}
		remaining -= used
		result = append(result, event)
		return remaining > 0
	}
	for _, line := range raw {
		var event remoteWireEvent
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		var message remoteWireMessage
		_ = json.Unmarshal(event.Message, &message)
		switch event.Type {
		case "user":
			if text := remoteUserText(message.Content); text != "" {
				if !appendEvent(remote.TranscriptEvent{Kind: "user", Text: text, At: event.Timestamp}) {
					return result, true
				}
			}
		case "assistant":
			var blocks []remoteWireBlock
			if json.Unmarshal(message.Content, &blocks) != nil {
				continue
			}
			for _, block := range blocks {
				switch block.Type {
				case "text":
					if block.Text != "" && !appendEvent(remote.TranscriptEvent{Kind: "assistant", Text: block.Text, At: event.Timestamp}) {
						return result, true
					}
				case "tool_use":
					name := remoteClip(block.Name, remoteTitleBytes)
					if name != "" && !appendEvent(remote.TranscriptEvent{Kind: "tool", ToolName: name, Summary: permission.Summary(name), At: event.Timestamp}) {
						return result, true
					}
				}
			}
		case "_lifecycle":
			if remoteLifecyclePhase(event.Phase) {
				if !appendEvent(remote.TranscriptEvent{Kind: "lifecycle", Text: "Agent " + event.Phase, At: event.Timestamp}) {
					return result, true
				}
			}
		}
	}
	return result, truncated
}

func remoteUserText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []remoteWireBlock
	if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 || blocks[0].Type != "text" {
		return ""
	}
	// buildUserContent always puts the human's text first and attachment bodies
	// after it. Deliberately read one block only: later text blocks can contain
	// a full attached file and must never reach a paired device.
	return blocks[0].Text
}

func remoteLifecyclePhase(phase string) bool {
	switch phase {
	case "started", "resumed", "exited", "interrupted", "turn_aborted", "error":
		return true
	default:
		return false
	}
}

func remoteClipToBudget(value string, max int) (string, bool) {
	if max <= 0 {
		return "", value != ""
	}
	if len(value) <= max {
		return value, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}

func remoteClip(value string, max int) string {
	clipped, _ := remoteClipToBudget(value, max)
	return clipped
}

func remoteCapLinesAndBytes(value string, maxLines, maxBytes int) (string, bool) {
	if maxLines <= 0 {
		maxLines = 2000
	}
	if maxBytes <= 0 {
		maxBytes = 128 * 1024
	}
	lines := strings.SplitAfter(value, "\n")
	if len(lines) > maxLines {
		value = strings.Join(lines[:maxLines], "")
		return remoteClip(value, maxBytes), true
	}
	clipped, wasClipped := remoteClipToBudget(value, maxBytes)
	return clipped, wasClipped
}
