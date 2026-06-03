package main

import (
	"fmt"
	"sort"
	"strings"
)

// FallbackSource describes a secondary Proxmox host whose instances are
// all surfaced as Protected and assigned to a single shared tier (named
// by TierName, numbered by TierNum). Used for "fallback" hosts that
// stay up while the primary cluster sleeps.
type FallbackSource struct {
	Name     string // host identifier — written to Instance.Source
	Client   ProxmoxAPI
	TierName string
	TierNum  int
}

// DiscoverInstances queries Proxmox for all VMs and LXCs on the primary
// host, then on each fallback host, and merges the results. Primary-host
// instances are matched to tier definitions by tag. Fallback-host
// instances bypass tag matching and land in a single tier per source,
// marked Protected so the orchestrator never starts or stops them.
func DiscoverInstances(primary ProxmoxAPI, fallbacks []FallbackSource, tierDefs []TierConfig) ([]Instance, map[int]string, error) {
	instances, err := discoverPrimary(primary, tierDefs)
	if err != nil {
		return nil, nil, err
	}

	tierNames := make(map[int]string, len(tierDefs)+len(fallbacks))
	for _, td := range tierDefs {
		tierNames[td.Tier] = td.Name
	}

	for _, fb := range fallbacks {
		fbInstances, err := discoverFallback(fb)
		if err != nil {
			return nil, nil, fmt.Errorf("fallback %q: %w", fb.Name, err)
		}
		instances = append(instances, fbInstances...)
		tierNames[fb.TierNum] = fb.TierName
	}

	// Sort by tier, then VMID for stable ordering
	sort.Slice(instances, func(i, j int) bool {
		if instances[i].Tier != instances[j].Tier {
			return instances[i].Tier < instances[j].Tier
		}
		return instances[i].VMID < instances[j].VMID
	})

	return instances, tierNames, nil
}

func discoverPrimary(client ProxmoxAPI, tierDefs []TierConfig) ([]Instance, error) {
	vms, err := client.ListVMs()
	if err != nil {
		return nil, fmt.Errorf("listing VMs: %w", err)
	}
	lxcs, err := client.ListLXCs()
	if err != nil {
		return nil, fmt.Errorf("listing LXCs: %w", err)
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

	return instances, nil
}

func discoverFallback(fb FallbackSource) ([]Instance, error) {
	vms, err := fb.Client.ListVMs()
	if err != nil {
		return nil, fmt.Errorf("listing VMs: %w", err)
	}
	lxcs, err := fb.Client.ListLXCs()
	if err != nil {
		return nil, fmt.Errorf("listing LXCs: %w", err)
	}

	all := append(vms, lxcs...)
	instances := make([]Instance, 0, len(all))
	for _, pi := range all {
		instances = append(instances, Instance{
			VMID:      pi.VMID,
			Name:      pi.Name,
			Type:      pi.Type,
			Tier:      fb.TierNum,
			Tags:      parseTags(pi.Tags),
			Source:    fb.Name,
			Protected: true,
		})
	}
	return instances, nil
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
