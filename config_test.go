package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	yaml := `
proxmox:
  url: "https://10.0.0.1:8006"
  node: "pve"
  token_id: "user@pve!tokenname"
  token_secret: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

tiers:
  - tag: "infra"
    tier: 1
    name: infra
  - tag: "local-omni"
    tier: 2
    name: local-omni
`
	path := writeTestFile(t, "config.yaml", yaml)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}

	if cfg.Proxmox.URL != "https://10.0.0.1:8006" {
		t.Errorf("URL = %q, want %q", cfg.Proxmox.URL, "https://10.0.0.1:8006")
	}
	if cfg.Proxmox.Node != "pve" {
		t.Errorf("Node = %q, want %q", cfg.Proxmox.Node, "pve")
	}
	if len(cfg.TierDefs) != 2 {
		t.Fatalf("got %d tiers, want 2", len(cfg.TierDefs))
	}
	if cfg.TierDefs[0].Tag != "infra" {
		t.Errorf("first tier tag = %q, want infra", cfg.TierDefs[0].Tag)
	}
	if cfg.TierDefs[1].Tier != 2 {
		t.Errorf("second tier number = %d, want 2", cfg.TierDefs[1].Tier)
	}
	if cfg.TierDefs[1].Name != "local-omni" {
		t.Errorf("second tier name = %q, want local-omni", cfg.TierDefs[1].Name)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"missing url", `
proxmox:
  node: pve
  token_id: a
  token_secret: b
tiers:
  - tag: "infra"
    tier: 1
    name: infra
`},
		{"missing node", `
proxmox:
  url: https://example.com
  token_id: a
  token_secret: b
tiers:
  - tag: "infra"
    tier: 1
    name: infra
`},
		{"no tiers", `
proxmox:
  url: https://example.com
  node: pve
  token_id: a
  token_secret: b
tiers: []
`},
		{"missing tag", `
proxmox:
  url: https://example.com
  node: pve
  token_id: a
  token_secret: b
tiers:
  - tier: 1
    name: infra
`},
		{"zero tier", `
proxmox:
  url: https://example.com
  node: pve
  token_id: a
  token_secret: b
tiers:
  - tag: "infra"
    tier: 0
    name: infra
`},
		{"duplicate tier number", `
proxmox:
  url: https://example.com
  node: pve
  token_id: a
  token_secret: b
tiers:
  - tag: "infra"
    tier: 1
    name: infra
  - tag: "apps"
    tier: 1
    name: apps
`},
		{"night_sleep under tier", `
proxmox:
  url: https://example.com
  node: pve
  token_id: a
  token_secret: b
tiers:
  - tag: "infra"
    tier: 1
    name: infra
    schedule:
      - name: bad
        cron: "0 23 * * *"
        action: night_sleep
`},
		{"wake at top level", `
proxmox:
  url: https://example.com
  node: pve
  token_id: a
  token_secret: b
tiers:
  - tag: "infra"
    tier: 1
    name: infra
schedule:
  - name: bad
    cron: "0 23 * * *"
    action: wake
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTestFile(t, "config.yaml", tt.yaml)
			_, err := LoadConfig(path)
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

// TestLoadConfigTierSchedule asserts wake/sleep entries under a tier load
// cleanly and end up on the right TierConfig.
func TestLoadConfigTierSchedule(t *testing.T) {
	yaml := `
proxmox:
  url: https://example.com
  node: pve
  token_id: a
  token_secret: b
tiers:
  - tag: "infra"
    tier: 1
    name: infra
    schedule:
      - name: infra-wake
        cron: "0 7 * * *"
        action: wake
      - name: infra-sleep
        cron: "0 23 * * *"
        action: sleep
        notify: "Tier 1 sleeping"
  - tag: "apps"
    tier: 2
    name: apps
`
	path := writeTestFile(t, "config.yaml", yaml)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.TierDefs[0].Schedule) != 2 {
		t.Fatalf("tier 1 schedule len = %d, want 2", len(cfg.TierDefs[0].Schedule))
	}
	if cfg.TierDefs[0].Schedule[0].Action != "wake" {
		t.Errorf("first action = %q, want wake", cfg.TierDefs[0].Schedule[0].Action)
	}
	if cfg.TierDefs[1].Schedule != nil {
		t.Errorf("tier 2 should have no schedule, got %v", cfg.TierDefs[1].Schedule)
	}
}

func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
