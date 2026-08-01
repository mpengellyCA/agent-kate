package kimi

import (
	"regexp"
	"strconv"
	"strings"
)

// Kimi answers `/usage` locally — no inference, no tokens billed — with a
// human-readable block rather than a machine shape, and the exact wording is
// not part of any protocol. So the readout is recovered by pattern, and every
// pattern is optional: an unrecognised layout yields no usage at all rather
// than a wrong number, and the context meter simply stays empty.

// tokenCount matches a formatted count: 1234, 1,234, 1 234, 12.5k, 200K, 1.2M.
const tokenCount = `(\d[\d,_ ]*(?:\.\d+)?)\s*([kKmM])?`

var (
	// "12,345 / 200,000" or "12.5k/128k" — used tokens over the window. The
	// most direct form, and the one worth trusting first.
	reFraction = regexp.MustCompile(tokenCount + `\s*(?:tokens?\s*)?/\s*` + tokenCount)
	// "Context window: 200,000" / "max context 128k".
	reWindow = regexp.MustCompile(`(?i)(?:context\s*window|max\s*context|window)\s*[:=]?\s*` + tokenCount)
	// "Context: 12,345 tokens" / "context used: 12k" / "total tokens: 12,345".
	rePrompt = regexp.MustCompile(
		`(?i)(?:context(?:\s*used)?|tokens?\s*used|used|total\s*tokens?|input(?:\s*tokens?)?)\s*[:=]\s*` + tokenCount)
	// "Output: 812" — only when the CLI splits the two.
	reOutput = regexp.MustCompile(`(?i)output(?:\s*tokens?)?\s*[:=]\s*` + tokenCount)
	// "(23%)" / "23% full" — the fallback when only a percentage is printed.
	rePercent = regexp.MustCompile(`(\d{1,3}(?:\.\d+)?)\s*%`)
	// ANSI colour the CLI may emit even on a pipe.
	reANSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

// parseCount turns one tokenCount match (digits plus an optional k/M suffix)
// into a whole number of tokens. Returns 0 when the digits make no sense.
func parseCount(digits, suffix string) int64 {
	clean := strings.NewReplacer(",", "", "_", "", " ", "").Replace(digits)
	f, err := strconv.ParseFloat(clean, 64)
	if err != nil || f < 0 {
		return 0
	}
	switch strings.ToLower(suffix) {
	case "k":
		f *= 1000
	case "m":
		f *= 1000 * 1000
	}
	return int64(f)
}

// parseUsage recovers a context readout from `/usage` output. ok is false when
// nothing usable was found.
func parseUsage(text string) (usageInfo, bool) {
	text = reANSI.ReplaceAllString(text, "")
	if strings.TrimSpace(text) == "" {
		return usageInfo{}, false
	}
	var u usageInfo

	// A "used / window" fraction is unambiguous, so take the first one whose
	// halves are ordered like a context fill (used ≤ window, window large
	// enough to be a window at all). That ordering test is what keeps a date
	// or a "3/5 steps" progress line from being read as a token count.
	fractions := reFraction.FindAllStringSubmatch(text, -1)
	for _, m := range fractions {
		used, window := parseCount(m[1], m[2]), parseCount(m[3], m[4])
		if window >= 1000 && used <= window {
			u.PromptTokens, u.ContextWindow = used, window
			break
		}
	}
	// The output looked like a fraction and none of them made sense as a
	// context fill: this is not the layout we think it is, and the labelled
	// fallbacks below would read the same misparsed numbers. Give up instead.
	if len(fractions) > 0 && u.ContextWindow == 0 {
		return usageInfo{}, false
	}
	if u.ContextWindow == 0 {
		if m := reWindow.FindStringSubmatch(text); m != nil {
			u.ContextWindow = parseCount(m[1], m[2])
		}
	}
	if u.PromptTokens == 0 {
		if m := rePrompt.FindStringSubmatch(text); m != nil {
			u.PromptTokens = parseCount(m[1], m[2])
		}
	}
	if m := reOutput.FindStringSubmatch(text); m != nil {
		u.OutputTokens = parseCount(m[1], m[2])
	}
	// Last resort: a percentage against a known window still gives the meter
	// the fill it needs, even though the absolute count was never printed.
	if u.PromptTokens == 0 && u.ContextWindow > 0 {
		if m := rePercent.FindStringSubmatch(text); m != nil {
			if pct, err := strconv.ParseFloat(m[1], 64); err == nil && pct >= 0 && pct <= 100 {
				u.PromptTokens = int64(float64(u.ContextWindow) * pct / 100)
			}
		}
	}
	// A prompt total larger than the window is a misread, not a full context.
	if u.ContextWindow > 0 && u.PromptTokens > u.ContextWindow {
		return usageInfo{}, false
	}
	return u, u.known()
}
