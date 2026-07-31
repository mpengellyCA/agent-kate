package agent

import "testing"

func TestParseClaudeModelList(t *testing.T) {
	out := "Current model: Opus 5 (1M context) (default)\n" +
		"Usage: /model <name>. Available: sonnet, opus, haiku, fable, best, " +
		"sonnet[1m], opus[1m], fable[1m], opusplan, default, or a full model ID.\n"

	got := parseClaudeModelList(out)
	if len(got) == 0 {
		t.Fatal("parsed no models")
	}

	byVal := map[string]string{}
	for _, m := range got {
		byVal[m.Value] = m.Name
	}
	// Aliases are captured verbatim as the --model value.
	for _, v := range []string{"sonnet", "opus", "haiku", "fable", "best", "opus[1m]", "opusplan", "default"} {
		if _, ok := byVal[v]; !ok {
			t.Errorf("missing alias %q; got %+v", v, got)
		}
	}
	// The "or a full model ID" prose must not leak in as an entry.
	for _, m := range got {
		if m.Value == "" || m.Value == "or a full model ID" {
			t.Errorf("bogus entry %+v", m)
		}
	}
	// Friendly labels.
	if byVal["opus"] != "Opus" {
		t.Errorf("opus label = %q, want Opus", byVal["opus"])
	}
	if byVal["opus[1m]"] != "Opus (1M)" {
		t.Errorf("opus[1m] label = %q, want Opus (1M)", byVal["opus[1m]"])
	}
	if byVal["default"] != "Default (Opus 5)" {
		t.Errorf("default label = %q, want Default (Opus 5)", byVal["default"])
	}
}

func TestParseClaudeModelListNoMarker(t *testing.T) {
	if got := parseClaudeModelList("not authenticated\n"); got != nil {
		t.Errorf("want nil on unrecognised output, got %+v", got)
	}
}
