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

func TestModelRoleMustBeKnown(t *testing.T) {
	snapshot := CatalogueSnapshot{ContractVersion: ContractVersion, HarnessID: "fixture",
		Models: []ModelDescriptor{{ID: "m1", DisplayName: "Model 1", Role: "imaginary"}}}
	snapshot.Revision = CatalogueRevision(snapshot)
	if err := ValidateCatalogue(snapshot); err == nil {
		t.Fatal("unknown model role was accepted")
	}
}

func TestDescriptorSerializesConservativeInteropMatrix(t *testing.T) {
	descriptor := HarnessDescriptor{ContractVersion: ContractVersion, ID: "fixture",
		DisplayName: "Fixture", Health: HealthOK,
		Operations: Operations(OperationCommands, OperationCompaction, OperationSubagentTranscripts)}

	matrix := descriptor.Interoperability()
	if matrix.Commands != InteropNative || matrix.Compaction.InPlace != InteropNative ||
		matrix.Compaction.Cold != InteropUnsupported || matrix.SubagentTranscripts != InteropNative {
		t.Fatalf("operation-derived matrix = %#v", matrix)
	}
	// A transcript reader is deliberately not mistaken for full lifecycle
	// support, and no unimplemented Codex-style interaction is inferred.
	for name, support := range map[string]InteropSupport{
		"continuation": matrix.Continuation, "plans": matrix.Plans, "tasks": matrix.Tasks,
		"subagents": matrix.Subagents, "questions": matrix.Questions, "dynamicTools": matrix.DynamicTools,
	} {
		if support != InteropUnsupported {
			t.Errorf("%s = %q; want unsupported", name, support)
		}
	}

	b, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ContractVersion int                    `json:"contractVersion"`
		Interop         InteroperabilityMatrix `json:"interop"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.ContractVersion != ContractVersion || wire.Interop.Commands != InteropNative ||
		wire.Interop.Compaction.InPlace != InteropNative || wire.Interop.Questions != InteropUnsupported {
		t.Fatalf("wire descriptor lost interop facts: %s", b)
	}
}

func TestDescriptorInteropAllowsOnlyKnownSupportLevels(t *testing.T) {
	descriptor := HarnessDescriptor{ContractVersion: ContractVersion, ID: "fixture", DisplayName: "Fixture",
		Health: HealthOK, Interop: InteroperabilityMatrix{Questions: "maybe"}}
	if err := ValidateDescriptor(descriptor); err == nil {
		t.Fatal("invalid interop support was accepted")
	}

	descriptor.Interop.Questions = InteropManaged
	if err := ValidateDescriptor(descriptor); err != nil {
		t.Fatalf("managed support rejected: %v", err)
	}

	// A matrix may add a new lifecycle dimension, but cannot make an existing
	// operation gate say the opposite thing to the rest of the core.
	descriptor.Operations = Operations(OperationCommands)
	descriptor.Interop.Commands = InteropUnsupported
	if err := ValidateDescriptor(descriptor); err == nil {
		t.Fatal("operation/matrix contradiction was accepted")
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
