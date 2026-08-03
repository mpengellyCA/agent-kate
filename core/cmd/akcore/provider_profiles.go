package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agentkate/internal/agent"
)

// providerProfileFile is the key-free profile mirror written by the desktop
// client. Credentials are never part of this file: a routed process resolves
// its token from the profile's named environment variable at spawn time.
type providerProfileFile struct {
	Profiles []providerProfile `json:"profiles"`
}

type providerProfile struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	BaseURL string            `json:"baseUrl"`
	EnvVar  string            `json:"envVar"`
	Models  map[string]string `json:"models"`
}

func providerProfilePaths() []string {
	if explicit := strings.TrimSpace(os.Getenv("AGENTKATE_PROVIDER_PROFILES")); explicit != "" {
		return []string{explicit}
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(config, "agentkate", "providers.json")}
}

// resolveProviderBinding turns an opaque provider id into the adapter-private
// runtime binding used for a launch. The public RPC and linkage DTOs contain
// only the id; neither profile URLs nor credentials cross that boundary.
func resolveProviderBinding(id string) (*agent.Provider, error) {
	id = strings.TrimSpace(id)
	if id == "" || id == "direct" || id == "claude-direct" {
		return nil, nil
	}
	// Built-in profiles are available before the desktop has written its
	// key-free mirror. Their credentials still come solely from the process
	// environment.
	for _, builtin := range []providerProfile{
		{ID: "fireworks", Name: "Fireworks (Fire Pass)",
			BaseURL: "https://api.fireworks.ai/inference", EnvVar: "FIREWORKS_API_KEY"},
		{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1",
			EnvVar: "OPENROUTER_API_KEY"},
	} {
		if id == builtin.ID {
			return &agent.Provider{ID: builtin.ID, Name: builtin.Name,
				BaseURL: builtin.BaseURL, EnvVar: builtin.EnvVar}, nil
		}
	}
	for _, path := range providerProfilePaths() {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read provider profiles: %w", err)
		}
		var profiles providerProfileFile
		if err := json.Unmarshal(raw, &profiles); err != nil {
			return nil, fmt.Errorf("read provider profiles: %w", err)
		}
		for _, profile := range profiles.Profiles {
			if profile.ID != id {
				continue
			}
			if strings.TrimSpace(profile.BaseURL) == "" {
				return nil, fmt.Errorf("provider %q has no endpoint", id)
			}
			if strings.TrimSpace(profile.EnvVar) == "" {
				return nil, fmt.Errorf("provider %q has no credential environment variable", id)
			}
			return &agent.Provider{ID: profile.ID, Name: profile.Name,
				BaseURL: profile.BaseURL, EnvVar: profile.EnvVar, Models: profile.Models}, nil
		}
	}
	return nil, fmt.Errorf("provider profile %q is not available to the core", id)
}
