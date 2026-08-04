package remote

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Backend is everything the remote server borrows from the core.
//
// It is an interface rather than a direct dependency because cmd/akcore is
// package main and cannot be imported. That constraint turns out to be the
// right shape anyway: the frozen /api/v1/ contract is deliberately NOT a mirror
// of the internal IPC vocabulary (the socket churns freely, a phone's cached
// PWA cannot), and this interface is the seam where the two vocabularies meet.
// Keeping it narrow is a security property: there is no Cowork verb and no
// caller-supplied filesystem root. The developer methods below remain behind
// per-device, default-deny capabilities and key their worktree to a thread the
// core resolves itself.
//
// Implementations must be safe for concurrent use and must respect ctx: an HTTP
// handler cancels it when the phone walks out of Wi-Fi range, and a backend that
// ignores that leaks a goroutine per abandoned request.
type Backend interface {
	// ListAgents returns the roster. It is called both by GET /agents and by the
	// SSE roster coalescer, so the two can never disagree about what a row says.
	ListAgents(ctx context.Context) ([]Agent, error)

	// Transcript returns a page of redacted, typed transcript events,
	// newest-last. It is deliberately not a transport for harness stream-json.
	Transcript(ctx context.Context, req TranscriptRequest) (Transcript, error)

	// Send is reserved until the core can publish one canonical user-turn echo
	// to desktop and remote transcripts. Its principal is session-derived.
	Send(ctx context.Context, principal Principal, req SendRequest) (SendResult, error)

	// Fork creates a fresh, isolated continuation of an existing thread. The
	// HTTPS handler admits it only for devices with CapAgentManage.
	Fork(ctx context.Context, principal Principal, req ForkRequest) (ForkResult, error)
	StartProjectAgent(ctx context.Context, principal Principal, req ProjectAgentRequest) (ProjectAgentResult, error)
	ProjectLaunchOptions(ctx context.Context, req ProjectLaunchOptionsRequest) (ProjectLaunchOptions, error)
	ListFiles(ctx context.Context, req FileRequest) ([]FileEntry, error)
	ReadFile(ctx context.Context, req FileRequest) (FileContent, error)
	ReadImage(ctx context.Context, req FileRequest) (ImageContent, error)
	WriteFile(ctx context.Context, principal Principal, req FileWriteRequest) (FileContent, error)

	// PermissionDetail returns the renderable content of ONE parked prompt.
	//
	// It exists because permissionRequested deliberately carries no `input`,
	// and the two prompt kinds a human most wants to answer from a phone —
	// ExitPlanMode and AskUserQuestion — keep the material you need to answer
	// them inside it. It must return ErrUnknownRequest for an id that is
	// unknown OR already resolved: a prompt that has been answered has no
	// detail to render, and pretending otherwise would put a live-looking
	// approve button on a lock screen.
	PermissionDetail(ctx context.Context, requestID string) (PermissionDetail, error)

	// RespondPermission answers a parked prompt. It must return one of
	// ErrUnknownRequest, ErrAlreadyResolved or ErrExpired when the answer did not
	// land — "you were too late" and "that never existed" are different messages
	// to someone staring at a stale lock-screen button. If the core cannot tell
	// them apart (Broker.Resolve only reports a bool), return ErrAlreadyResolved:
	// it is the more useful of the two and by far the more likely.
	RespondPermission(ctx context.Context, principal Principal, ans PermissionAnswer) error

	// Interrupt stops the current turn, leaving the thread alive.
	Interrupt(ctx context.Context, principal Principal, threadID string) error

	// Stop shuts the thread's process down gracefully. It is deliberately the
	// most destructive verb reachable from a phone: nothing here discards an
	// agent, deletes a worktree or edits a file.
	Stop(ctx context.Context, principal Principal, threadID string) error

	// Diff returns the thread's worktree diff, already capped core-side.
	Diff(ctx context.Context, req DiffRequest) (Diff, error)

	// GitStatus and GitLog are passed through verbatim as the JSON body. They are
	// `any` on purpose: the contract does not freeze their shape, so pinning a
	// struct here would force a lockstep change in three repositories the first
	// time git.status grows a field.
	GitStatus(ctx context.Context, threadID string) (any, error)
	GitLog(ctx context.Context, threadID string, limit int) (any, error)
}

// Principal is constructed from a verified HTTPS session. It is immutable
// attribution, never an IPC UI role and never a caller-supplied JSON field.
type Principal struct {
	DeviceID   string
	DeviceName string
	SessionID  string
}

// Sentinel errors the Backend returns so the HTTP layer can choose a status code
// and a stable machine-readable `error` string. Anything else becomes a 500.
var (
	// ErrUnknownThread → 404 unknown-thread.
	ErrUnknownThread = errors.New("remote: unknown thread")
	// ErrUnknownRequest → 404 unknown-request.
	ErrUnknownRequest = errors.New("remote: unknown permission request")
	// ErrAlreadyResolved → 409 already-resolved.
	ErrAlreadyResolved = errors.New("remote: permission already resolved")
	// ErrExpired → 410 expired.
	ErrExpired = errors.New("remote: permission expired")
	// ErrBusy → 409 busy (only reachable with mode="reject").
	ErrBusy = errors.New("remote: thread is mid-turn")
	// ErrUnsupported → 501 unsupported; the thread's harness lacks the capability.
	ErrUnsupported = errors.New("remote: unsupported for this harness")
	// ErrNotListening is returned by MintDevice when remote access is off. A
	// pairing URL is only meaningful with a listener behind it; minting one
	// anyway produces a QR code pointing at whatever else holds that port.
	ErrNotListening = errors.New("remote: remote access is not switched on")
	// ErrConflict means a revisioned resource changed after the caller read it.
	ErrConflict = errors.New("remote: revision conflict")
)

// Agent is one roster row, in Go types. The wire form is built in handlers.go so
// redaction and formatting happen in exactly one place.
type Agent struct {
	ThreadID string
	Title    string
	// Project is a DISPLAY NAME. The wire encoder strips anything that looks
	// like a path, because "no route takes a filesystem path" is worth nothing
	// if a response hands one out.
	Project    string
	Backend    string // harness id, for a badge — never for behaviour
	EngineName string // display string from the harness registry
	Model      string
	Status     string // running | dormant | archived (PROCESS state)
	Busy       bool   // TURN state, from TurnTracker
	// AwaitingPermission is nil when nothing is parked. A client diffing rows
	// needs to see the field go to null, not merely stop being sent.
	AwaitingPermission *Awaiting
	// Attention is computed core-side on purpose: "which of my eight agents
	// needs me" must not be a client-side rule two clients can disagree about.
	Attention      bool
	LastActivityAt time.Time
	ParentThreadID string
	Role           string // "" | controller | worker
}

// Awaiting describes a parked permission prompt.
//
// There is deliberately no raw tool input here. permission.requested carries
// `input` on the local socket because the desktop needs it to render
// ExitPlanMode markdown and AskUserQuestion forms; nothing that leaves this
// machine may carry it, so the struct that crosses the network simply does not
// have the field. Summary is the core-computed, already-redacted digest.
type Awaiting struct {
	RequestID string
	Kind      string // tool | question | plan
	ToolName  string
	Summary   string
	Deadline  time.Time
}

// PermissionDetail is one parked prompt, in enough detail to answer it.
//
// Kind decides which of the last two fields is populated, and the rule has no
// exceptions: "plan" carries Plan, "question" carries Questions, and "tool" —
// the Bash command line the whole redaction discipline exists for — carries
// NEITHER. A remote caller looking at a tool prompt gets the core-computed
// Summary and nothing else, exactly as before this route existed.
type PermissionDetail struct {
	RequestID string
	ThreadID  string
	Kind      string // tool | question | plan
	ToolName  string
	Summary   string
	Deadline  time.Time
	// Plan is ExitPlanMode's markdown; populated only when Kind == "plan".
	Plan string
	// Questions is AskUserQuestion's list, verbatim; populated only when
	// Kind == "question". An answer echoes it back unchanged and keys its
	// answers by each question's own text, so anything less than verbatim is
	// unanswerable rather than merely lossy.
	Questions json.RawMessage
}

// TranscriptRequest is a page request. Before is an opaque cursor the backend
// minted; the remote server never interprets it.
type TranscriptRequest struct {
	ThreadID string
	Limit    int
	Before   string
	MaxBytes int
}

// TranscriptEvent is the allowlisted remote projection of one transcript row.
// Tool arguments and raw permission input have no field here by design.
type TranscriptEvent struct {
	Kind     string `json:"kind"` // user | assistant | tool | lifecycle
	Text     string `json:"text,omitempty"`
	ToolName string `json:"toolName,omitempty"`
	Summary  string `json:"summary,omitempty"`
	// Attachments is a deliberately generic, body-free marker for a human turn.
	// A paired device may learn that an image or text file was attached, but not
	// its filename, path, contents, or a browser URL capable of retrieving it.
	Attachments []TranscriptAttachment `json:"attachments,omitempty"`
	At          time.Time              `json:"at,omitempty"`
}

// TranscriptAttachment is the remote-safe representation of an attachment.
// Keep this separate from the upload DTO: it must stay incapable of carrying
// the filename or bytes back through transcript or SSE delivery.
type TranscriptAttachment struct {
	Kind string `json:"kind"` // "image" or "text"
}

// Transcript is one page of a thread's remote-safe conversation projection.
type Transcript struct {
	Events []TranscriptEvent
	// Truncated reports that a single oversized result was clipped, matching
	// M0.4's shape. It is emitted unconditionally on the wire, even when false:
	// a field that only appears on the bad path is a field nobody handles.
	Truncated bool
	// HasMore reports that older events exist beyond this page.
	HasMore bool
	// NextBefore is the cursor to feed back as ?before=. Empty when !HasMore.
	NextBefore string
}

// SendRequest carries a prompt to a thread.
type SendRequest struct {
	ThreadID string
	Text     string
	// Attachments are a deliberately narrow upload DTO. They have no path,
	// provenance, URL, or arbitrary metadata field: a paired browser can offer
	// bytes to an agent, never name a desktop file for the core to read.
	Attachments []Attachment
	// Mode is M0.3's core-side semantics: "queue" | "reject". The HTTP layer
	// never passes "now" — see sendModeNow in handlers.go for why.
	Mode string
}

// Attachment is one browser-uploaded text or image file. Validation happens at
// the HTTP boundary before it reaches this DTO; keeping it separate from the
// desktop's agent.Attachment prevents path/cache metadata from becoming part of
// the remote contract by accident.
type Attachment struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Text      string `json:"text"`
	DataB64   string `json:"dataB64"`
}

// SendResult is what the core did with the prompt.
type SendResult struct {
	Queued bool
	// Position is the 1-based slot in the thread's follow-up queue, 0 when the
	// message went straight to the agent.
	Position int
	// Resuming reports that the thread was asleep and is being woken to take
	// this message, which arrives once the session is back. The desktop does
	// the same thing — its Send button reads "Resume agent" on a dormant
	// thread — so a phone that merely refused would be the weaker client for
	// no reason. It is surfaced because "sent" and "sent, and your agent is
	// starting up" deserve different words on screen.
	Resuming bool
}

type ForkRequest struct {
	ThreadID string
	Title    string
}

type ForkResult struct{ ThreadID string }

// ProjectAgentRequest is seeded from an existing project member. The paired
// browser cannot supply a workspace, provider, environment, or Cowork state.
type ProjectAgentRequest struct {
	ThreadID   string `json:"threadId,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	Title      string `json:"title,omitempty"`
	Backend    string `json:"backend,omitempty"`
	ProviderID string `json:"providerId,omitempty"`
	Model      string `json:"model,omitempty"`
	Effort     string `json:"effort,omitempty"`
	Isolation  string `json:"isolation,omitempty"`
}
type ProjectAgentResult struct{ ThreadID string }
type ProjectLaunchOptionsRequest struct{ ThreadID, Backend, ProviderID string }
type LaunchChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type LaunchModel struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Efforts []string `json:"efforts"`
}

// ProjectLaunchOptions is consumed directly by the remote agent-launch form.
//
// Browser-facing DTOs must always declare their exact JSON names. Go's default
// exported-field encoding (for example, "Harnesses") silently leaves a client
// that expects `harnesses` with empty controls. This has regressed twice; keep
// the exact-wire-shape test in handlers_test.go when changing this type.
type ProjectLaunchOptions struct {
	Harnesses     []LaunchChoice      `json:"harnesses"`
	Providers     []LaunchChoice      `json:"providers"`
	WorktreeModes []LaunchChoice      `json:"worktreeModes"`
	Models        []LaunchModel       `json:"models"`
	Default       ProjectAgentRequest `json:"default"`
}
type FileRequest struct{ ThreadID, Path string }
type FileWriteRequest struct{ ThreadID, Path, Text, Revision string }

// FileContent is the text editor payload. These JSON names are part of the
// browser contract; see ProjectLaunchOptions for the regression note on never
// relying on Go's default exported-field names for browser-facing DTOs.
type FileContent struct {
	Path     string `json:"path"`
	Text     string `json:"text"`
	Revision string `json:"revision"`
}

// ImageContent is an allowlisted worktree preview. It is kept separate from
// FileContent so a browser never mistakes arbitrary binary data for editable
// UTF-8 text.
type ImageContent struct {
	MediaType string
	Data      []byte
}

// FileEntry is a direct child of a worktree directory. Paths are relative and
// never include Agent Kate's own metadata directories.
type FileEntry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
	Size      int64  `json:"size,omitempty"`
}

// PermissionAnswer is a human decision arriving from a phone.
type PermissionAnswer struct {
	RequestID string
	Allow     bool
	// DenyMessage is the optional reason shown to the agent on a refusal.
	DenyMessage string
	// UpdatedInput carries AskUserQuestion answers and ExitPlanMode's plan back
	// to the harness. It is validated as a JSON object before it gets here.
	UpdatedInput json.RawMessage
}

// DiffRequest asks for a thread's worktree diff.
type DiffRequest struct {
	ThreadID string
	MaxBytes int
	MaxLines int
}

// Diff is a capped worktree diff.
type Diff struct {
	// Files may be left nil: the wire encoder derives the per-file stat from
	// Patch when it is, so an adapter can be a straight passthrough of the
	// core's agent.diff reply (which returns a patch and no file list).
	Files []DiffFile
	Patch string
	// Truncated reports the patch was cut to fit the caps.
	Truncated bool
	// OmittedFiles is emitted only when Truncated, per the frozen contract.
	OmittedFiles int
}

// DiffFile is one changed path in a diff.
type DiffFile struct {
	Path      string
	Status    string // M | A | D | R
	Additions int
	Deletions int
}
