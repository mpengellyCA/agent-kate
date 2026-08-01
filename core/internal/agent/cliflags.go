package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"time"
)

// The two launch flags that are NOT load-bearing: without them the stream is
// still a valid stream-json stream, it just carries less. Everything else in
// buildStartArgs' fixed prefix is either ancient (--print, --verbose) or
// structurally required (--output-format / --input-format), so gating those
// would buy nothing.
//
//   - --include-partial-messages adds `stream_event` lines. Absent, the UI
//     simply never paints a provisional row and renders the authoritative
//     `assistant` event when it lands — exactly what replaying a stored
//     transcript already does, since transcripts hold no stream_events.
//   - --forward-subagent-text tags subagent output onto the parent stream.
//     Absent, subagent turns are still visible through the on-disk subagent
//     transcripts (SubagentTranscripts), just not live.
//
// Both are recent additions. Appending them unconditionally makes the newest
// claude the hard minimum for EVERY launch — an older CLI rejects the unknown
// option and dies during spawn, which reaches the human as an opaque start
// failure with no mention of a version. The optional sweep flags
// (--fallback-model, --add-dir, …) are already conditional on the caller
// asking for them; these two are conditional on the binary knowing them.
const (
	flagIncludePartialMessages = "--include-partial-messages"
	flagForwardSubagentText    = "--forward-subagent-text"
)

// flagAppendSystemPromptFile takes a PATH instead of the prompt text, which is
// how the persona stays out of /proc/<pid>/cmdline (audit F23). Gated like the
// two above — an older claude dies on an unknown option — but with the opposite
// consequence on absence: the persona falls back to the inline argv flag, so
// the thread still gets its persona, just less privately.
//
// Verified live on claude 2.1.220: `--append-system-prompt-file /nonexistent`
// answers "Append system prompt file not found", where an invented flag answers
// "error: unknown option". It appears in `claude -p --help` only in the
// bracketed shorthand `--append-system-prompt[-file]`, which is why
// parseCLIFlags has to expand that notation.
const flagAppendSystemPromptFile = "--append-system-prompt-file"

// flagProbeTimeout bounds the `claude -p --help` probe. Help output is local
// and instant; anything past this is a wedged or non-responsive binary, and the
// answer then is "assume supported" (see supportedFlags) rather than stalling a
// launch the human is waiting on.
const flagProbeTimeout = 8 * time.Second

// cliFlags is the set of long option names the installed `claude` advertises in
// its own help output.
//
// A nil / empty set means NOT PROBED (or a probe that told us nothing), and
// supports() then answers true for everything. That direction is deliberate: a
// failed probe has taught us nothing about the binary, and every currently
// shipping claude has these flags, so degrading the stream on a probe hiccup
// would punish the common case to protect the rare one. The case this feature
// exists for — an old binary — is a probe that SUCCEEDS and simply does not
// list the flag.
type cliFlags map[string]bool

// supports reports whether the probed CLI accepts flag (with its leading
// dashes). Unprobed sets answer true; see the type comment.
func (f cliFlags) supports(flag string) bool {
	if len(f) == 0 {
		return true
	}
	return f[flag]
}

// flagProbeResult is one cached probe, keyed by the identity of the binary it
// probed so that upgrading claude underneath a running core re-probes instead
// of serving a stale vocabulary.
type flagProbeResult struct {
	key    string
	flags  cliFlags
	probed bool
}

// claudeBinKey identifies the binary a probe result belongs to: resolved path,
// size and mtime. Cheap (one stat, no subprocess) and it changes on any
// upgrade or reinstall, which is what "cache per version" needs in practice —
// asking the CLI its version would cost a second subprocess per launch to learn
// the same thing.
func claudeBinKey(bin string) string {
	path, err := exec.LookPath(bin)
	if err != nil {
		return bin
	}
	st, err := os.Stat(path)
	if err != nil {
		return path
	}
	return fmt.Sprintf("%s|%d|%d", path, st.Size(), st.ModTime().UnixNano())
}

// longFlagRE matches a long option as it appears in help output. Anchored on
// the two dashes; the trailing class stops at '=', ',', '<' and whitespace, so
// "--model <model>" and "--effort=<level>" both yield the bare name.
var longFlagRE = regexp.MustCompile(`--[a-zA-Z][a-zA-Z0-9-]*`)

// optionalSuffixFlagRE matches the help's "one name standing for two" notation,
// e.g. "--append-system-prompt[-file]" — the only place claude 2.1.220
// advertises --append-system-prompt-file. longFlagRE stops at the '[' and would
// see just the base name, so the file form would look unsupported on a binary
// that has it, and the persona would stay in argv forever.
var optionalSuffixFlagRE = regexp.MustCompile(`(--[a-zA-Z][a-zA-Z0-9-]*)\[(-[a-zA-Z0-9-]+)\]`)

// parseCLIFlags extracts the long-option vocabulary from help output.
func parseCLIFlags(help string) cliFlags {
	found := longFlagRE.FindAllString(help, -1)
	if len(found) == 0 {
		return nil
	}
	flags := make(cliFlags, len(found))
	for _, f := range found {
		flags[f] = true
	}
	// Expand the bracketed form: "--x[-file]" advertises both "--x" and
	// "--x-file". The base name is already in the map from longFlagRE.
	for _, m := range optionalSuffixFlagRE.FindAllStringSubmatch(help, -1) {
		flags[m[1]+m[2]] = true
	}
	return flags
}

// supportedFlags returns the installed CLI's long-option vocabulary, probing it
// at most once per binary identity.
//
// This reuses the existing option-discovery pattern rather than adding a second
// one: like DiscoverModels it is a bounded, best-effort subprocess whose failure
// mode is "return nothing and let the caller keep its defaults", and like the
// kimi supervisor's DiscoverOptions cache it answers from memory after the
// first call. It is deliberately NOT wired into harness.DiscoverOptions, which
// serves the UI a *user-facing* vocabulary (models / modes / efforts); the flag
// set is an internal launch detail with no picker behind it.
func (s *Supervisor) supportedFlags() cliFlags {
	key := claudeBinKey(s.claudeBin)

	s.flagMu.Lock()
	if s.flagCache.probed && s.flagCache.key == key {
		flags := s.flagCache.flags
		s.flagMu.Unlock()
		return flags
	}
	s.flagMu.Unlock()

	// Probe OUTSIDE the lock: a wedged binary would otherwise hold every
	// concurrent Start behind flagProbeTimeout. A racing second probe is
	// harmless (same answer, and the write below is last-one-wins).
	ctx, cancel := context.WithTimeout(context.Background(), flagProbeTimeout)
	defer cancel()
	// `-p --help`: the print-mode help page, which is where the streaming and
	// session options are listed. CombinedOutput because some CLIs print help
	// on stderr, and the output is parsed even when the exit code is non-zero —
	// several help implementations exit 1.
	out, _ := exec.CommandContext(ctx, s.claudeBin, "-p", "--help").CombinedOutput()
	flags := parseCLIFlags(string(out))

	s.flagMu.Lock()
	s.flagCache = flagProbeResult{key: key, flags: flags, probed: true}
	s.flagMu.Unlock()

	if flags == nil {
		s.log.Debug("claude flag probe returned nothing; launching with all flags",
			"bin", s.claudeBin)
	} else {
		for _, f := range []string{flagIncludePartialMessages, flagForwardSubagentText} {
			if !flags[f] {
				s.log.Info("claude does not advertise this flag; omitting it",
					"flag", f, "bin", s.claudeBin)
			}
		}
	}
	return flags
}
