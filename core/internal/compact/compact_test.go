package compact

import "testing"

func TestStrategyValid(t *testing.T) {
	known := []Strategy{
		ExitOpusHot, ExitSonnetCold,
		ResumeOpusCold, ResumeHaikuCold, ResumeSonnetCold, ResumeLocal,
	}
	for _, s := range known {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []Strategy{"", "garbage", "exit_opus", "Opus"} {
		if s.Valid() {
			t.Errorf("%q should not be valid", s)
		}
	}
}

func TestStrategyResolveDefaultsEmpty(t *testing.T) {
	if got := Strategy("").Resolve(); got != Default {
		t.Errorf("empty strategy should resolve to Default, got %q", got)
	}
	if got := Strategy("garbage").Resolve(); got != Default {
		t.Errorf("invalid strategy should resolve to Default, got %q", got)
	}
	if got := ResumeLocal.Resolve(); got != ResumeLocal {
		t.Errorf("valid strategy should resolve to itself, got %q", got)
	}
}

func TestStrategyRunsOnExit(t *testing.T) {
	cases := map[Strategy]bool{
		ExitOpusHot:      true,
		ExitSonnetCold:   true,
		ResumeOpusCold:   false,
		ResumeHaikuCold:  false,
		ResumeSonnetCold: false,
		ResumeLocal:      false,
		"":               true, // empty resolves to Default (ExitOpusHot)
	}
	for s, want := range cases {
		if got := s.RunsOnExit(); got != want {
			t.Errorf("%q.RunsOnExit() = %v, want %v", s, got, want)
		}
	}
}

func TestStrategyRunsOnResume(t *testing.T) {
	cases := map[Strategy]bool{
		ExitOpusHot:      false,
		ExitSonnetCold:   false,
		ResumeOpusCold:   true,
		ResumeHaikuCold:  true,
		ResumeSonnetCold: true,
		ResumeLocal:      true,
	}
	for s, want := range cases {
		if got := s.RunsOnResume(); got != want {
			t.Errorf("%q.RunsOnResume() = %v, want %v", s, got, want)
		}
	}
}

func TestStrategyModel(t *testing.T) {
	cases := map[Strategy]string{
		ExitOpusHot:      "", // hot path reuses the live process
		ResumeLocal:      "", // no LLM
		ResumeOpusCold:   "claude-opus-4-7",
		ExitSonnetCold:   "claude-sonnet-4-6",
		ResumeSonnetCold: "claude-sonnet-4-6",
		ResumeHaikuCold:  "claude-haiku-4-5-20251001",
	}
	for s, want := range cases {
		if got := s.Model(); got != want {
			t.Errorf("%q.Model() = %q, want %q", s, got, want)
		}
	}
}
