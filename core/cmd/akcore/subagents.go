package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"agentkate/internal/harness"
	"agentkate/internal/ipc"
)

// subagentTranscriber is implemented by harnesses whose CLI writes a separate
// on-disk conversation per subagent. It is an OPTIONAL interface, asserted at
// the call site (the same pattern as modelDiscoverer) rather than a method on
// harness.Harness: a backend without subagent files should not have to carry a
// stub, and the SubagentTranscripts descriptor operation is what the UI gates on.
type subagentTranscriber interface {
	SubagentTranscripts(threadID, sessionID string) ([]harness.SubagentTranscript, error)
}

// scanSubagentDir lists JSONL transcripts under dir, one per subagent.
//
// Two layouts, both probed on the real CLIs (plan 16 P6):
//
//   - claude 2.1.220: <project>/<session>/subagents/agent-<id>.jsonl — one file
//     per subagent, in the same shape as the main transcript.
//   - kimi 0.30.0: <session-dir>/agents/<id>/wire.jsonl — one DIRECTORY per
//     subagent (ids "main", "agent-0", "agent-1", …), holding the engine's own
//     wire protocol rather than a Claude-shaped transcript.
//
// label is filled by the caller when the file records one.
func scanSubagentDir(dir string, nested bool) []harness.SubagentTranscript {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []harness.SubagentTranscript
	for _, e := range entries {
		name := e.Name()
		if nested {
			if !e.IsDir() || name == "main" {
				continue // "main" is the thread itself, not a subagent
			}
			path := filepath.Join(dir, name, "wire.jsonl")
			if _, err := os.Stat(path); err != nil {
				continue
			}
			out = append(out, harness.SubagentTranscript{ID: name, Path: path})
			continue
		}
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		out = append(out, harness.SubagentTranscript{
			ID:   strings.TrimSuffix(name, ".jsonl"),
			Path: filepath.Join(dir, name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// kimiSubagentLabel reads the subagent's profile name out of the head of its
// wire log (a "config.update" line carries profileName: coder / explore /
// plan). Best-effort: an unreadable or profile-less file just has no label.
func kimiSubagentLabel(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	// The profile lands within the first handful of lines; stop early rather
	// than decoding a transcript that can run to megabytes.
	for i := 0; i < 12; i++ {
		var line struct {
			Type        string `json:"type"`
			ProfileName string `json:"profileName"`
		}
		if err := dec.Decode(&line); err != nil {
			return ""
		}
		if line.Type == "config.update" && line.ProfileName != "" {
			return line.ProfileName
		}
	}
	return ""
}

func registerSubagentHandlers(d handlerDeps) {
	// agent.subagentTranscripts lists the on-disk conversations of the
	// subagents a thread delegated to, so the UI can tail one. Gated on the
	// harness capability, and served by the adapter that knows its CLI's
	// layout — the UI never computes a CLI's private paths.
	//
	// SECURITY (audit F34 pass 4): UI-only, the same data class as
	// agent.transcript, which F34 gated. It answers for ANY thread named on the
	// wire, and what it hands back is the on-disk PATH of each subagent
	// conversation — a caller that has those paths and can read files has the
	// transcripts themselves, gate or no gate on agent.transcript. Its only
	// caller is ui/src/AgentPanel.cpp.
	d.srv.Handle("agent.subagentTranscripts",
		func(ctx context.Context, raw json.RawMessage) (any, error) {
			if err := requireUIWindow(d.srv, ctx); err != nil {
				return nil, err
			}
			var p struct {
				ThreadID string `json:"threadId"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, err.Error())
			}
			rec, ok := d.sessions.Get(p.ThreadID)
			if !ok {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "unknown thread "+p.ThreadID)
			}
			h := d.harnessFor(p.ThreadID)
			descriptor := h.Descriptor()
			if !descriptor.Supports(harness.OperationSubagentTranscripts) {
				return nil, unsupported("Subagent transcripts", descriptor)
			}
			finder, ok := h.(subagentTranscriber)
			if !ok {
				return nil, unsupported("Subagent transcripts", descriptor)
			}
			list, err := finder.SubagentTranscripts(p.ThreadID, rec.SessionID)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, err.Error())
			}
			if list == nil {
				list = []harness.SubagentTranscript{}
			}
			return map[string]any{"transcripts": list}, nil
		})
}
