package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveServer(t *testing.T) {
	tests := []struct {
		name      string
		flag      string
		env       string
		configYAM string // written to $XDG_CONFIG_HOME/homelab-manager/config.yaml
		want      string
	}{
		{
			name: "flag wins over everything",
			flag: "https://flag.example.com",
			env:  "https://env.example.com",
			configYAM: "server: https://config.example.com\n",
			want: "https://flag.example.com",
		},
		{
			name: "env wins over config",
			flag: "",
			env:  "https://env.example.com",
			configYAM: "server: https://config.example.com\n",
			want: "https://env.example.com",
		},
		{
			name: "config wins over default",
			flag: "",
			env:  "",
			configYAM: "server: https://config.example.com\n",
			want: "https://config.example.com",
		},
		{
			name:      "default fallback",
			flag:      "",
			env:       "",
			configYAM: "",
			want:      "http://localhost:8080",
		},
		{
			name:      "missing config file",
			flag:      "",
			env:       "",
			configYAM: "__SKIP__",
			want:      "http://localhost:8080",
		},
		{
			name:      "invalid YAML in config file",
			flag:      "",
			env:       "",
			configYAM: ":::not yaml",
			want:      "http://localhost:8080",
		},
		{
			name:      "config file with no server key",
			flag:      "",
			env:       "",
			configYAM: "something_else: true\n",
			want:      "http://localhost:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up XDG_CONFIG_HOME in a temp dir
			tmpDir := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", tmpDir)
			t.Setenv("HOMELAB_SERVER", tt.env)

			if tt.configYAM != "__SKIP__" && tt.configYAM != "" {
				cfgDir := filepath.Join(tmpDir, "homelab-manager")
				if err := os.MkdirAll(cfgDir, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(tt.configYAM), 0644); err != nil {
					t.Fatal(err)
				}
			}

			got := ResolveServer(tt.flag)
			if got != tt.want {
				t.Errorf("ResolveServer(%q) = %q, want %q", tt.flag, got, tt.want)
			}
		})
	}
}

func TestClientConfigPath(t *testing.T) {
	t.Run("respects XDG_CONFIG_HOME", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-test")
		got := clientConfigPath()
		want := "/tmp/xdg-test/homelab-manager/config.yaml"
		if got != want {
			t.Errorf("clientConfigPath() = %q, want %q", got, want)
		}
	})

	t.Run("falls back to ~/.config", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", "")
		got := clientConfigPath()
		home, _ := os.UserHomeDir()
		want := filepath.Join(home, ".config", "homelab-manager", "config.yaml")
		if got != want {
			t.Errorf("clientConfigPath() = %q, want %q", got, want)
		}
	})
}
