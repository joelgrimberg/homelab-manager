package main

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const defaultServer = "http://localhost:8080"

// ClientConfig holds CLI-side configuration (server URL, etc.).
type ClientConfig struct {
	Server string `yaml:"server"`
}

// clientConfigPath returns ~/.config/homelab-manager/config.yaml,
// respecting $XDG_CONFIG_HOME if set.
func clientConfigPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "homelab-manager", "config.yaml")
}

// LoadClientConfig reads the client config file. It returns a zero-value
// ClientConfig (no error) if the file is missing or unparseable — the
// caller should treat missing fields as "not configured".
func LoadClientConfig() ClientConfig {
	path := clientConfigPath()
	if path == "" {
		return ClientConfig{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ClientConfig{}
	}
	var cfg ClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ClientConfig{}
	}
	return cfg
}

// ResolveServer applies the priority chain:
//
//	-server flag  >  $HOMELAB_SERVER env  >  config file  >  http://localhost:8080
//
// Pass the raw flag value (empty string means "not set by user").
func ResolveServer(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("HOMELAB_SERVER"); env != "" {
		return env
	}
	if cfg := LoadClientConfig(); cfg.Server != "" {
		return cfg.Server
	}
	return defaultServer
}
