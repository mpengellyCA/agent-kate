package main

// This file is the narrow seam between the HTTPS transport and the current
// core. It deliberately does not speak IPC: a verified paired device is a
// remote-human principal, never a UI window and never an agent bridge.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"agentkate/internal/agent"
	"agentkate/internal/fsperm"
	"agentkate/internal/gitstatus"
	"agentkate/internal/harness"
	"agentkate/internal/permission"
	"agentkate/internal/remote"
	"agentkate/internal/safe"
	"agentkate/internal/session"
	"agentkate/internal/worktree"
)

type remoteBackend struct {
	d             handlerDeps
	attachmentDir string // test override; production uses a private session sibling.
}

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
	if b.d.remote != nil {
		projected = b.d.remote.mergeHumanEchoes(req.ThreadID, projected)
	}
	return remote.Transcript{Events: projected, Truncated: truncated}, nil
}

// Send reaches the canonical human-surface operation after HTTPS authentication
// has constructed an immutable remote principal. It never calls a harness
// directly, so remote queueing and cross-surface echo cannot drift from the
// desktop path.
func (b *remoteBackend) Send(ctx context.Context, principal remote.Principal, req remote.SendRequest) (remote.SendResult, error) {
	if err := ctx.Err(); err != nil {
		return remote.SendResult{}, err
	}
	if err := b.requireThread(req.ThreadID); err != nil {
		return remote.SendResult{}, err
	}
	atts := make([]agent.Attachment, 0, len(req.Attachments))
	for _, att := range req.Attachments {
		converted := agent.Attachment{
			Kind: att.Kind, Name: att.Name, MediaType: att.MediaType, Text: att.Text, DataB64: att.DataB64,
		}
		// Image bytes were supplied by the paired device, not by a desktop path.
		// Keep one owner-private copy so the trusted desktop can render the same
		// thumbnail after the live event or a restart. This cache path is carried
		// only in desktop events/sidecars; remote projections get generic markers.
		if att.Kind == "image" {
			converted.CachePath = b.cacheRemoteImage(att)
		}
		atts = append(atts, converted)
	}
	result, err := b.d.humanSend(humanPrincipal{Surface: remoteSurface, Device: principal.DeviceName}, req.ThreadID, req.Text, atts)
	if err != nil {
		return remote.SendResult{}, fmt.Errorf("remote send: %w", err)
	}
	return remote.SendResult{Queued: result.Queued, Position: result.Position, Resuming: result.Resuming}, nil
}

func remoteAttachmentMarkers(atts []agent.Attachment) []remote.TranscriptAttachment {
	if len(atts) == 0 {
		return nil
	}
	markers := make([]remote.TranscriptAttachment, 0, len(atts))
	for _, att := range atts {
		if att.Kind == "image" || att.Kind == "text" {
			markers = append(markers, remote.TranscriptAttachment{Kind: att.Kind})
		}
	}
	return markers
}

func (b *remoteBackend) cacheRemoteImage(att remote.Attachment) string {
	if att.Kind != "image" {
		return ""
	}
	data, err := base64.StdEncoding.DecodeString(att.DataB64)
	if err != nil || len(data) == 0 {
		return ""
	}
	ext := map[string]string{
		"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp",
	}[att.MediaType]
	if ext == "" {
		return ""
	}
	dir := b.attachmentDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(session.DefaultPath()), "remote-attachments")
	}
	if err := fsperm.MkdirAll(dir); err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	path := filepath.Join(dir, fmt.Sprintf("%x.%s", sum[:], ext))
	// O_EXCL prevents following a pre-existing symlink. When another request
	// already published this content-addressed image, HardenFile confirms it is
	// a regular owner-private file before it is re-used.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fsperm.FileMode)
	if err == nil {
		if _, err = f.Write(data); err == nil {
			err = f.Close()
		} else {
			_ = f.Close()
		}
		if err == nil {
			if _, err = fsperm.HardenFile(path); err == nil {
				return path
			}
		}
		_ = os.Remove(path)
		return ""
	}
	if !os.IsExist(err) {
		return ""
	}
	info, statErr := os.Lstat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return ""
	}
	if _, err = fsperm.HardenFile(path); err != nil {
		return ""
	}
	return path
}

// Fork starts a server-resolved continuation. The remote request never chooses
// a worktree, model, provider, or Cowork state. Cowork is intentionally not
// inherited: developer remote authority must not bootstrap desktop control.
func (b *remoteBackend) Fork(ctx context.Context, _ remote.Principal, req remote.ForkRequest) (remote.ForkResult, error) {
	if err := ctx.Err(); err != nil {
		return remote.ForkResult{}, err
	}
	if err := b.requireThread(req.ThreadID); err != nil {
		return remote.ForkResult{}, err
	}
	src, _ := b.d.sessions.Get(req.ThreadID)
	h := b.d.harnessFor(req.ThreadID)
	if !h.Descriptor().Supports(harness.OperationFork) {
		return remote.ForkResult{}, remote.ErrUnsupported
	}
	if src.SessionID == "" {
		return remote.ForkResult{}, fmt.Errorf("this agent has no conversation yet to fork")
	}
	src.CoworkEnabled = false
	threadID := agent.NewThreadID()
	safe.Go("remote.forkThread", func() { forkAgentThread(b.d, h, src, threadID, "", "", req.Title) })
	return remote.ForkResult{ThreadID: threadID}, nil
}

// StartProjectAgent is intentionally a much narrower cousin of the desktop's
// agent.start. A paired device can name an existing project member and provide
// its first instruction, but it cannot choose a path, credentials, an
// environment overlay, or Cowork. Those trusted settings are copied only from
// the server-resolved seed record, with Cowork force-disabled.
func (b *remoteBackend) StartProjectAgent(ctx context.Context, _ remote.Principal, req remote.ProjectAgentRequest) (remote.ProjectAgentResult, error) {
	if err := ctx.Err(); err != nil {
		return remote.ProjectAgentResult{}, err
	}
	if err := b.requireThread(req.ThreadID); err != nil {
		return remote.ProjectAgentResult{}, err
	}
	src, _ := b.d.sessions.Get(req.ThreadID)
	if strings.TrimSpace(src.Project) == "" {
		return remote.ProjectAgentResult{}, fmt.Errorf("this agent has no project")
	}
	h := b.d.harnessFor(req.ThreadID)
	descriptor := h.Descriptor()
	threadID := agent.NewThreadID()
	sessionID := ""
	if descriptor.Supports(harness.OperationMintSessionID) {
		sessionID = session.NewID()
	}
	p := agentStartParams{
		WorkspacePath: src.Project, Prompt: req.Prompt, PermissionMode: src.PermissionMode,
		SandboxMode: src.EffectiveSettings.SandboxMode, Effort: src.Effort, Model: src.Model,
		Backend: descriptor.ID, Isolation: "", CoworkEnabled: false, Provider: providerFromRecord(src),
		SystemPrompt: src.SystemPrompt, Agents: src.Agents, Env: src.Env,
		FallbackModels: src.FallbackModels, DisallowedTools: src.DisallowedTools, AddDirs: src.AddDirs,
		StrictMCPConfig: src.StrictMCPConfig, MaxBudgetUSD: src.MaxBudgetUSD,
	}
	if p.PermissionMode == "" {
		p.PermissionMode = src.EffectiveSettings.PermissionMode
	}
	if p.Effort == "" {
		p.Effort = src.EffectiveSettings.ReasoningEffort
	}
	if p.Model == "" {
		p.Model = src.EffectiveSettings.Model
	}
	if b.d.turns != nil {
		b.d.turns.TurnQueued(threadID)
	}
	safe.Go("remote.startProjectAgent", func() {
		_, _, _ = launchThread(b.d, h, threadID, sessionID, p, launchMeta{Title: req.Title})
	})
	return remote.ProjectAgentResult{ThreadID: threadID}, nil
}

func (b *remoteBackend) remoteFile(req remote.FileRequest) (string, error) {
	if err := b.requireThread(req.ThreadID); err != nil {
		return "", err
	}
	rec, _ := b.d.sessions.Get(req.ThreadID)
	clean := filepath.Clean(req.Path)
	if req.Path == "" || filepath.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid worktree path")
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	if parts[0] == ".git" || parts[0] == ".agentkate" {
		return "", fmt.Errorf("that worktree path is protected")
	}
	path := filepath.Join(rec.Worktree.Path, clean)
	root, err := filepath.EvalSymlinks(rec.Worktree.Path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the worktree")
	}
	return resolved, nil
}

func (b *remoteBackend) remoteDirectory(req remote.FileRequest) (string, error) {
	if err := b.requireThread(req.ThreadID); err != nil {
		return "", err
	}
	rec, _ := b.d.sessions.Get(req.ThreadID)
	clean := filepath.Clean(req.Path)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid worktree path")
	}
	if req.Path == "" || clean == "." {
		clean = "."
	}
	if clean != "." {
		parts := strings.Split(filepath.ToSlash(clean), "/")
		if parts[0] == ".git" || parts[0] == ".agentkate" {
			return "", fmt.Errorf("that worktree path is protected")
		}
	}
	root, err := filepath.EvalSymlinks(rec.Worktree.Path)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(root, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the worktree")
	}
	return path, nil
}

func (b *remoteBackend) ListFiles(ctx context.Context, req remote.FileRequest) ([]remote.FileEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := b.remoteDirectory(req)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]remote.FileEntry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Name() == ".git" || entry.Name() == ".agentkate" || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		rel := entry.Name()
		if req.Path != "" && filepath.Clean(req.Path) != "." {
			rel = filepath.Join(req.Path, entry.Name())
		}
		out = append(out, remote.FileEntry{Path: filepath.ToSlash(rel), Name: entry.Name(), Directory: info.IsDir(), Size: info.Size()})
		if len(out) == 256 {
			break
		}
	}
	return out, nil
}
func remoteRevision(b []byte) string { sum := sha256.Sum256(b); return fmt.Sprintf("%x", sum[:]) }
func (b *remoteBackend) ReadFile(ctx context.Context, req remote.FileRequest) (remote.FileContent, error) {
	if err := ctx.Err(); err != nil {
		return remote.FileContent{}, err
	}
	path, err := b.remoteFile(req)
	if err != nil {
		return remote.FileContent{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return remote.FileContent{}, err
	}
	if len(data) > 512*1024 || !utf8.Valid(data) {
		return remote.FileContent{}, fmt.Errorf("file is not readable UTF-8 text")
	}
	return remote.FileContent{Path: filepath.ToSlash(filepath.Clean(req.Path)), Text: string(data), Revision: remoteRevision(data)}, nil
}
func (b *remoteBackend) WriteFile(ctx context.Context, _ remote.Principal, req remote.FileWriteRequest) (remote.FileContent, error) {
	if err := ctx.Err(); err != nil {
		return remote.FileContent{}, err
	}
	path, err := b.remoteFile(remote.FileRequest{ThreadID: req.ThreadID, Path: req.Path})
	if err != nil {
		return remote.FileContent{}, err
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return remote.FileContent{}, err
	}
	if req.Revision == "" || req.Revision != remoteRevision(old) {
		return remote.FileContent{}, remote.ErrConflict
	}
	if err := os.WriteFile(path, []byte(req.Text), 0o600); err != nil {
		return remote.FileContent{}, err
	}
	return b.ReadFile(ctx, remote.FileRequest{ThreadID: req.ThreadID, Path: req.Path})
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
			if text, attachments := remoteUserProjection(message.Content); text != "" {
				if !appendEvent(remote.TranscriptEvent{Kind: "user", Text: text, Attachments: attachments, At: event.Timestamp}) {
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
	text, _ := remoteUserProjection(content)
	return text
}

func remoteUserProjection(content json.RawMessage) (string, []remote.TranscriptAttachment) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text, nil
	}
	var blocks []remoteWireBlock
	if json.Unmarshal(content, &blocks) != nil || len(blocks) == 0 {
		return "", nil
	}
	// buildUserContent always puts the human's text first and attachment bodies
	// after it. Deliberately read one block only: later text blocks can contain
	// a full attached file and must never reach a paired device.
	if blocks[0].Type == "text" && !strings.HasPrefix(blocks[0].Text, "Attached file `") {
		text = blocks[0].Text
	}
	// Inspect only block types. Names and bodies have no route to this DTO.
	attachments := make([]remote.TranscriptAttachment, 0, len(blocks))
	for _, block := range blocks {
		switch {
		case block.Type == "image":
			attachments = append(attachments, remote.TranscriptAttachment{Kind: "image"})
		case block.Type == "text" && strings.HasPrefix(block.Text, "Attached file `"):
			attachments = append(attachments, remote.TranscriptAttachment{Kind: "text"})
		}
	}
	if text != "" {
		return text, attachments
	}
	if len(attachments) == 0 {
		return "", nil
	}
	// An attachment-only prompt has no safe prose block. Preserve the shape
	// without projecting a filename, attachment body, or image data.
	return fmt.Sprintf("Attached %d file(s)", len(attachments)), attachments
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
