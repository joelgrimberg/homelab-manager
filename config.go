package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type ProxmoxConfig struct {
	URL         string `yaml:"url"`
	Node        string `yaml:"node"`
	TokenID     string `yaml:"token_id"`
	TokenSecret string `yaml:"token_secret"`
}

type TierConfig struct {
	Tag  string `yaml:"tag"`
	Tier int    `yaml:"tier"`
	Name string `yaml:"name"`
}

// Instance is a runtime type representing a discovered Proxmox VM or LXC.
type Instance struct {
	VMID int
	Name string
	Type string // "qemu" or "lxc"
	Tier int
}

type Config struct {
	Proxmox  ProxmoxConfig `yaml:"proxmox"`
	TierDefs []TierConfig  `yaml:"tiers"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Proxmox.URL == "" {
		return fmt.Errorf("proxmox.url is required")
	}
	if c.Proxmox.Node == "" {
		return fmt.Errorf("proxmox.node is required")
	}
	if c.Proxmox.TokenID == "" {
		return fmt.Errorf("proxmox.token_id is required")
	}
	if c.Proxmox.TokenSecret == "" {
		return fmt.Errorf("proxmox.token_secret is required")
	}
	if len(c.TierDefs) == 0 {
		return fmt.Errorf("at least one tier is required")
	}
	seen := map[int]bool{}
	for i, td := range c.TierDefs {
		if td.Tag == "" {
			return fmt.Errorf("tiers[%d]: tag is required", i)
		}
		if td.Tier < 1 {
			return fmt.Errorf("tiers[%d]: tier must be >= 1", i)
		}
		if seen[td.Tier] {
			return fmt.Errorf("tiers[%d]: duplicate tier number %d", i, td.Tier)
		}
		seen[td.Tier] = true
	}
	return nil
}
