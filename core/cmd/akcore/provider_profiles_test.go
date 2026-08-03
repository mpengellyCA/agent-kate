package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProviderBindingUsesKeyFreeProfileMirror(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(`{"profiles":[{"id":"test","name":"Test","baseUrl":"https://example.test/v1","envVar":"TEST_PROVIDER_KEY","models":{"main":"test-model"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTKATE_PROVIDER_PROFILES", path)
	got, err := resolveProviderBinding("test")
	if err != nil {
		t.Fatalf("resolveProviderBinding: %v", err)
	}
	if got == nil || got.BaseURL != "https://example.test/v1" || got.EnvVar != "TEST_PROVIDER_KEY" || got.AuthToken != "" {
		t.Fatalf("binding = %#v, want key-free test profile", got)
	}
	if _, err := resolveProviderBinding("missing"); err == nil {
		t.Fatal("missing profile resolved")
	}
}
