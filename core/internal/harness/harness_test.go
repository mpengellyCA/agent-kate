package harness

import (
	"encoding/json"
	"testing"

	"agentkate/internal/agent"
)

// fakeHarness is a do-nothing Harness carrying only an id.
type fakeHarness struct{ id string }

func (f fakeHarness) Capabilities() Capabilities              { return Capabilities{ID: f.id} }
func (f fakeHarness) Launch(StartSpec) (Launched, error)      { return Launched{}, nil }
func (f fakeHarness) Send(string, string, []agent.Attachment) error { return nil }
func (f fakeHarness) Interrupt(string) error                  { return nil }
func (f fakeHarness) Stop(string) error                       { return nil }
func (f fakeHarness) Running(string) bool                     { return false }
func (f fakeHarness) StopAll()                                {}
func (f fakeHarness) ReadTranscript(string, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (f fakeHarness) SetOption(string, string, string) (string, error) { return "", nil }
func (f fakeHarness) DiscoverOptions() ([]DiscoveredOption, error)     { return nil, nil }
func (f fakeHarness) BrowseSessions() ([]BrowsableSession, error)      { return nil, nil }

func TestRegistryLookupAndOrder(t *testing.T) {
	r := NewRegistry("claude")
	r.Register(fakeHarness{id: "claude"})
	r.Register(fakeHarness{id: "kimi"})

	// The empty id (persisted records' default backend) resolves to the default.
	h, ok := r.Get("")
	if !ok || h.Capabilities().ID != "claude" {
		t.Fatalf(`Get("") = %v, %v; want the claude default`, h, ok)
	}
	if h, ok := r.Get("kimi"); !ok || h.Capabilities().ID != "kimi" {
		t.Fatalf(`Get("kimi") = %v, %v; want the kimi harness`, h, ok)
	}
	if _, ok := r.Get("antigravity"); ok {
		t.Fatal(`Get("antigravity") succeeded; want a miss for an unregistered id`)
	}

	// All preserves registration order — it is the engine-picker order.
	all := r.All()
	if len(all) != 2 || all[0].Capabilities().ID != "claude" || all[1].Capabilities().ID != "kimi" {
		t.Fatalf("All() order wrong: %v", all)
	}

	// Re-registering an id replaces it without duplicating the order entry.
	r.Register(fakeHarness{id: "kimi"})
	if len(r.All()) != 2 {
		t.Fatalf("re-registration duplicated the order: %d entries", len(r.All()))
	}
}
