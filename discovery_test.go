package main

import (
	"testing"
)

func TestDiscoverInstances(t *testing.T) {
	mock := &mockProxmox{
		statuses: map[int]string{},
		vms: []ProxmoxInstance{
			{VMID: 100, Name: "dns-primary", Status: "running", Tags: "infra", Type: "qemu"},
			{VMID: 200, Name: "cp-1", Status: "running", Tags: "control-plane", Type: "qemu"},
			{VMID: 201, Name: "cp-2", Status: "stopped", Tags: "control-plane", Type: "qemu"},
			{VMID: 999, Name: "untagged", Status: "running", Tags: "", Type: "qemu"},
		},
		lxcs: []ProxmoxInstance{
			{VMID: 101, Name: "dns-secondary", Status: "running", Tags: "infra", Type: "lxc"},
		},
	}

	tierDefs := []TierConfig{
		{Tag: "infra", Tier: 1, Name: "infra"},
		{Tag: "control-plane", Tier: 2, Name: "control-plane"},
	}

	instances, tierNames, err := DiscoverInstances(mock, tierDefs)
	if err != nil {
		t.Fatalf("DiscoverInstances() error: %v", err)
	}

	if len(instances) != 4 {
		t.Fatalf("got %d instances, want 4", len(instances))
	}

	// Should be sorted by tier, then VMID
	if instances[0].VMID != 100 || instances[0].Tier != 1 {
		t.Errorf("instances[0] = {VMID:%d, Tier:%d}, want {100, 1}", instances[0].VMID, instances[0].Tier)
	}
	if instances[1].VMID != 101 || instances[1].Tier != 1 {
		t.Errorf("instances[1] = {VMID:%d, Tier:%d}, want {101, 1}", instances[1].VMID, instances[1].Tier)
	}
	if instances[2].VMID != 200 || instances[2].Tier != 2 {
		t.Errorf("instances[2] = {VMID:%d, Tier:%d}, want {200, 2}", instances[2].VMID, instances[2].Tier)
	}
	if instances[3].VMID != 201 || instances[3].Tier != 2 {
		t.Errorf("instances[3] = {VMID:%d, Tier:%d}, want {201, 2}", instances[3].VMID, instances[3].Tier)
	}

	if tierNames[1] != "infra" {
		t.Errorf("tierNames[1] = %q, want infra", tierNames[1])
	}
	if tierNames[2] != "control-plane" {
		t.Errorf("tierNames[2] = %q, want control-plane", tierNames[2])
	}

	// Tags should be populated on every discovered instance.
	for i, inst := range instances {
		if len(inst.Tags) == 0 {
			t.Errorf("instances[%d] (VMID %d): Tags empty, want populated", i, inst.VMID)
		}
	}
}

func TestDiscoverInstancesNoMatches(t *testing.T) {
	mock := &mockProxmox{
		statuses: map[int]string{},
		vms: []ProxmoxInstance{
			{VMID: 100, Name: "untagged", Status: "running", Tags: "", Type: "qemu"},
		},
	}

	tierDefs := []TierConfig{
		{Tag: "infra", Tier: 1, Name: "infra"},
	}

	instances, _, err := DiscoverInstances(mock, tierDefs)
	if err != nil {
		t.Fatalf("DiscoverInstances() error: %v", err)
	}

	if len(instances) != 0 {
		t.Errorf("got %d instances, want 0", len(instances))
	}
}

func TestDiscoverInstancesSemicolonTags(t *testing.T) {
	mock := &mockProxmox{
		statuses: map[int]string{},
		vms: []ProxmoxInstance{
			{VMID: 100, Name: "multi-tag", Status: "running", Tags: "other;infra", Type: "qemu"},
		},
	}

	tierDefs := []TierConfig{
		{Tag: "infra", Tier: 1, Name: "infra"},
	}

	instances, _, err := DiscoverInstances(mock, tierDefs)
	if err != nil {
		t.Fatalf("DiscoverInstances() error: %v", err)
	}

	if len(instances) != 1 {
		t.Fatalf("got %d instances, want 1", len(instances))
	}
	if instances[0].VMID != 100 {
		t.Errorf("VMID = %d, want 100", instances[0].VMID)
	}
}

func TestDiscoverInstancesCommaSeparated(t *testing.T) {
	mock := &mockProxmox{
		statuses: map[int]string{},
		vms: []ProxmoxInstance{
			{VMID: 100, Name: "multi-tag", Status: "running", Tags: "other,infra", Type: "qemu"},
		},
	}

	tierDefs := []TierConfig{
		{Tag: "infra", Tier: 1, Name: "infra"},
	}

	instances, _, err := DiscoverInstances(mock, tierDefs)
	if err != nil {
		t.Fatalf("DiscoverInstances() error: %v", err)
	}

	if len(instances) != 1 {
		t.Fatalf("got %d instances, want 1", len(instances))
	}
}

func TestDiscoverInstancesMultiTag(t *testing.T) {
	// One tier (3) matches three alternative tags. A VM carrying only
	// the "alternate" `machine-request.homelab-control-planes` tag — as
	// emitted by the Omni provider — should still land in tier 3, even
	// though the canonical `homelab` tag is absent.
	mock := &mockProxmox{
		statuses: map[int]string{},
		vms: []ProxmoxInstance{
			{VMID: 103, Name: "hl-cp-canon", Status: "running", Tags: "homelab;homelab-cp", Type: "qemu"},
			{VMID: 106, Name: "hl-cp-provider", Status: "running", Tags: "go-proxmox+cloud-init;machine-request.homelab-control-planes", Type: "qemu"},
			{VMID: 120, Name: "hl-worker", Status: "running", Tags: "machine-request.homelab-workers", Type: "qemu"},
		},
	}
	tierDefs := []TierConfig{
		{Tier: 3, Name: "homelab", Tags: []string{
			"homelab",
			"machine-request.homelab-control-planes",
			"machine-request.homelab-workers",
		}},
	}
	instances, _, err := DiscoverInstances(mock, tierDefs)
	if err != nil {
		t.Fatalf("DiscoverInstances() error: %v", err)
	}
	if len(instances) != 3 {
		t.Fatalf("got %d instances, want 3", len(instances))
	}
	for _, inst := range instances {
		if inst.Tier != 3 {
			t.Errorf("VMID %d landed in tier %d, want 3", inst.VMID, inst.Tier)
		}
	}
}

func TestTierConfigAllTags(t *testing.T) {
	cases := []struct {
		name string
		t    TierConfig
		want []string
	}{
		{"tags-wins", TierConfig{Tag: "old", Tags: []string{"a", "b"}}, []string{"a", "b"}},
		{"tag-fallback", TierConfig{Tag: "old"}, []string{"old"}},
		{"empty", TierConfig{}, nil},
	}
	for _, c := range cases {
		got := c.t.AllTags()
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s[%d]: got %q want %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseTags(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"infra", []string{"infra"}},
		{"infra;workers", []string{"infra", "workers"}},
		{"infra,workers", []string{"infra", "workers"}},
		{"infra ; workers", []string{"infra", "workers"}},
		{" infra , workers ", []string{"infra", "workers"}},
		{"a;;b", []string{"a", "b"}},
	}

	for _, tt := range tests {
		got := parseTags(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("parseTags(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseTags(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}
