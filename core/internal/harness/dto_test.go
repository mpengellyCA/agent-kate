package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDescriptorAndCatalogueValidation(t *testing.T) {
	descriptor := HarnessDescriptor{ContractVersion: ContractVersion, ID: "fixture",
		DisplayName: "Fixture", Health: HealthOK,
		Operations: Operations(OperationFork, OperationCompaction)}
	if err := ValidateDescriptor(descriptor); err != nil {
		t.Fatal(err)
	}
	snapshot := CatalogueSnapshot{ContractVersion: ContractVersion, HarnessID: "fixture",
		Models: []ModelDescriptor{{ID: "m1", DisplayName: "Model 1", SupportedReasoningEfforts: []string{"low", "high"}}},
		Settings: []SettingDescriptor{{Key: SettingModel, DisplayName: "Model", Timing: TimingLaunch},
			{Key: SettingReasoningEffort, DisplayName: "Reasoning", Timing: TimingNextTurn}}}
	snapshot.Revision = CatalogueRevision(snapshot)
	if err := ValidateCatalogue(snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Settings = append(snapshot.Settings, snapshot.Settings[0])
	if err := ValidateCatalogue(snapshot); err == nil {
		t.Fatal("duplicate setting was accepted")
	}
}

func TestAgentLaunchDoesNotSerializeRuntimeBindings(t *testing.T) {
	launch := AgentLaunch{Ref: AgentRef{ThreadID: "t1", HarnessID: "fixture", NativeSessionID: "native"},
		WorkDir: "/project", Prompt: "go", Settings: AgentSettings{Model: "m1"}}
	b, err := json.Marshal(launch)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret", "credential", "environment", "provider", "mcp"} {
		if strings.Contains(strings.ToLower(string(b)), forbidden) {
			t.Fatalf("launch DTO leaked runtime binding %q: %s", forbidden, b)
		}
	}
}
