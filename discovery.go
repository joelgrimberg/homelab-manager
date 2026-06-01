package main

import (
	"fmt"
	"sort"
	"strings"
)

// DiscoverInstances queries Proxmox for all VMs and LXCs, then matches
// them to tier definitions by tag. Returns the discovered instances and
// a tier number → name map.
func DiscoverInstances(client ProxmoxAPI, tierDefs []TierConfig) ([]Instance, map[int]string, error) {
	vms, err := client.ListVMs()
	if err != nil {
		return nil, nil, fmt.Errorf("listing VMs: %w", err)
	}
	lxcs, err := client.ListLXCs()
	if err != nil {
		return nil, nil, fmt.Errorf("listing LXCs: %w", err)
	}

	// Build tag → tier definition lookup. Each tier may match more than
	// one tag (e.g. both the canonical name and any provisioner-specific
	// `machine-request.*` tag), so iterate AllTags().
	tagToTier := make(map[string]TierConfig)
	for _, td := range tierDefs {
		for _, tag := range td.AllTags() {
			tagToTier[tag] = td
		}
	}

	var instances []Instance
	seen := map[int]bool{}
	all := append(vms, lxcs...)

	for _, pi := range all {
		tags := parseTags(pi.Tags)
		for _, tag := range tags {
			td, ok := tagToTier[tag]
			if !ok {
				continue
			}
			// One row per VM even if it matches multiple of a tier's tags
			// (e.g. both `homelab` and `machine-request.homelab-*`).
			if seen[pi.VMID] {
				break
			}
			seen[pi.VMID] = true
			instances = append(instances, Instance{
				VMID: pi.VMID,
				Name: pi.Name,
				Type: pi.Type,
				Tier: td.Tier,
				Tags: tags,
			})
		}
	}

	// Sort by tier, then VMID for stable ordering
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Tier != instances[j].Tier {
			return instances[i].Tier < instances[j].Tier
		}
		return instances[i].VMID < instances[j].VMID
	})

	tierNames := make(map[int]string, len(tierDefs))
	for _, td := range tierDefs {
		tierNames[td.Tier] = td.Name
	}

	return instances, tierNames, nil
}

// parseTags splits a Proxmox tags string (semicolon or comma-separated)
// into individual trimmed tags.
func parseTags(raw string) []string {
	if raw == "" {
		return nil
	}
	// Proxmox uses semicolons by default, but commas are also common
	raw = strings.ReplaceAll(raw, ",", ";")
	parts := strings.Split(raw, ";")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}
