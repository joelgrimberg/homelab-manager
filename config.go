package main

import (
	"fmt"
	"os"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
	"gopkg.in/yaml.v3"
)

type ProxmoxConfig struct {
	URL         string `yaml:"url"`
	Node        string `yaml:"node"`
	TokenID     string `yaml:"token_id"`
	TokenSecret string `yaml:"token_secret"`
}

// ExtraProxmoxConfig describes a secondary Proxmox host whose VMs are
// surfaced in the UI but never started/stopped by the orchestrator
// ("fallback" hosts that stay up while the primary cluster sleeps).
// All instances from one ExtraProxmoxConfig land in a single tier named
// by TierName, with Protected=true.
type ExtraProxmoxConfig struct {
	URL         string `yaml:"url"`
	Node        string `yaml:"node"`
	TokenID     string `yaml:"token_id"`
	TokenSecret string `yaml:"token_secret"`
	TierName    string `yaml:"tier_name,omitempty"` // default: Node
}

func (e ExtraProxmoxConfig) ToProxmoxConfig() ProxmoxConfig {
	return ProxmoxConfig{
		URL:         e.URL,
		Node:        e.Node,
		TokenID:     e.TokenID,
		TokenSecret: e.TokenSecret,
	}
}

type TierConfig struct {
	// Tag is the legacy single-tag form. Tags is the new multi-tag form
	// used when a tier should match several different Proxmox tags (for
	// example when an external provisioner emits a tag like
	// `machine-request.foo` that we want lumped into the same tier as
	// the hand-applied `foo`). Either field works; if both are set,
	// Tags wins.
	Tag  string   `yaml:"tag,omitempty"`
	Tags []string `yaml:"tags,omitempty"`
	Tier int      `yaml:"tier"`
	Name string   `yaml:"name"`
	// ManualOnly opts the tier out of global `night_wake` (and therefore
	// the big Awake/Night toggle in the PWA). Instances in this tier
	// will not be auto-started when night mode is exited. Per-tier
	// scheduled wakes and explicit WakeTier calls (master toggle on the
	// tier's card / CLI / per-tier API) still work.
	ManualOnly bool `yaml:"manual_only,omitempty"`
	// Schedule entries fire wake/sleep against this tier only. Actions
	// allowed here are "wake", "sleep", and "" (notify-only). Read at
	// startup; not editable via /api/schedule (which writes the global
	// list only).
	Schedule []ScheduleEntry `yaml:"schedule,omitempty"`
}

// AllTags returns the union of Tags and Tag — the full set of Proxmox
// tags that should map to this tier.
func (t TierConfig) AllTags() []string {
	if len(t.Tags) > 0 {
		return t.Tags
	}
	if t.Tag != "" {
		return []string{t.Tag}
	}
	return nil
}

type NightModeConfig struct {
	KeepAwakeTags []string `yaml:"keep_awake_tags"`
}

type WebPushConfig struct {
	VAPIDPublic  string `yaml:"vapid_public_key"`
	VAPIDPrivate string `yaml:"vapid_private_key"`
	VAPIDSubject string `yaml:"vapid_subject"`
}

// ScheduleEntry is one cron-driven job. Allowed values for Action depend
// on where the entry lives: top-level `schedule:` permits "night_sleep",
// "night_wake", or ""; an entry under `tiers[].schedule` permits "wake",
// "sleep", or "". Notify, if non-empty, is sent as a push when the entry
// fires.
//
// SnoozeTarget/SnoozeMinutes/WarnBefore are optional and turn the notify
// push into an actionable one with a Snooze button targeting another entry
// (typically "night-sleep"). With SnoozeTarget empty the entry behaves
// exactly as before.
type ScheduleEntry struct {
	Name          string `yaml:"name" json:"name"`
	Cron          string `yaml:"cron" json:"cron"`
	Action        string `yaml:"action,omitempty" json:"action,omitempty"`
	Notify        string `yaml:"notify,omitempty" json:"notify,omitempty"`
	SnoozeTarget  string `yaml:"snooze_target,omitempty" json:"snooze_target,omitempty"`
	SnoozeMinutes int    `yaml:"snooze_minutes,omitempty" json:"snooze_minutes,omitempty"`
	WarnBefore    string `yaml:"warn_before,omitempty" json:"warn_before,omitempty"`
}

// Instance is a runtime type representing a discovered Proxmox VM or LXC.
// Source identifies which Proxmox host owns the instance; empty means the
// primary host. Protected instances are never started or stopped by the
// orchestrator (used for fallback hosts and never_touch-tagged VMs).
type Instance struct {
	VMID      int
	Name      string
	Type      string // "qemu" or "lxc"
	Tier      int
	Tags      []string
	Source    string // host name, e.g. "pxmx"; empty for primary
	Protected bool   // never started or stopped by orchestrator
}

type Config struct {
	Proxmox        ProxmoxConfig        `yaml:"proxmox"`
	ExtraProxmox   []ExtraProxmoxConfig `yaml:"extra_proxmox,omitempty"`
	TierDefs       []TierConfig         `yaml:"tiers"`
	NightMode      NightModeConfig      `yaml:"night_mode"`
	NeverTouchTags []string             `yaml:"never_touch_tags"`
	WebPush        WebPushConfig        `yaml:"web_push"`
	Schedule       []ScheduleEntry      `yaml:"schedule"`
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

	if cfg.WebPush.VAPIDSubject == "" {
		cfg.WebPush.VAPIDSubject = "joelgrimberg@gmail.com"
	}
	// webpush-go prepends `mailto:` itself when the subscriber isn't an
	// https URL — passing `mailto:foo@bar` produces a double prefix that
	// Apple's push servers reject with BadJwtToken. Strip it if present.
	cfg.WebPush.VAPIDSubject = strings.TrimPrefix(cfg.WebPush.VAPIDSubject, "mailto:")
	if cfg.WebPush.VAPIDPublic == "" || cfg.WebPush.VAPIDPrivate == "" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, fmt.Errorf("generating VAPID keys: %w", err)
		}
		cfg.WebPush.VAPIDPublic = pub
		cfg.WebPush.VAPIDPrivate = priv
		if err := writeWebPushToConfig(path, cfg.WebPush); err != nil {
			return nil, fmt.Errorf("persisting VAPID keys: %w", err)
		}
	}

	return &cfg, nil
}

// writeWebPushToConfig adds/replaces the `web_push` section in config.yaml
// while preserving the rest of the file (other sections, comments, ordering).
func writeWebPushToConfig(path string, wp WebPushConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}
	doc := root.Content[0]

	var wpNode yaml.Node
	if err := wpNode.Encode(wp); err != nil {
		return err
	}
	upsertMapKey(doc, "web_push", &wpNode)

	out, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// upsertMapKey inserts or replaces a key on a yaml mapping node.
func upsertMapKey(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	mapping.Content = append(mapping.Content, keyNode, value)
}

// WriteTierScheduleToConfig replaces the `schedule:` subkey on a specific
// tier in config.yaml. Finds the tier by its numeric `tier:` field. Other
// tiers, the top-level `schedule:`, and other sections are preserved.
// Removing all entries (nil/empty slice) drops the subkey on that tier.
func WriteTierScheduleToConfig(path string, tier int, entries []ScheduleEntry) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}
	doc := root.Content[0]

	tiersNode := mapValue(doc, "tiers")
	if tiersNode == nil || tiersNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("tiers: missing or not a sequence")
	}

	for _, item := range tiersNode.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		tn := mapValue(item, "tier")
		if tn == nil || tn.Value != fmt.Sprintf("%d", tier) {
			continue
		}
		if len(entries) == 0 {
			deleteMapKey(item, "schedule")
		} else {
			var schedNode yaml.Node
			if err := schedNode.Encode(entries); err != nil {
				return err
			}
			upsertMapKey(item, "schedule", &schedNode)
		}
		out, err := yaml.Marshal(&root)
		if err != nil {
			return err
		}
		return os.WriteFile(path, out, 0o600)
	}
	return fmt.Errorf("tier %d not found in config", tier)
}

// mapValue returns the value node for `key` in a mapping, or nil if absent.
func mapValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// deleteMapKey removes a key (and its value) from a yaml mapping node.
func deleteMapKey(mapping *yaml.Node, key string) {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// WriteScheduleToConfig replaces the `schedule:` section in config.yaml
// with the given entries, preserving the rest of the file.
func WriteScheduleToConfig(path string, entries []ScheduleEntry) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return err
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}
	doc := root.Content[0]

	var schedNode yaml.Node
	if err := schedNode.Encode(entries); err != nil {
		return err
	}
	upsertMapKey(doc, "schedule", &schedNode)

	out, err := yaml.Marshal(&root)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
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
		if len(td.AllTags()) == 0 {
			return fmt.Errorf("tiers[%d]: at least one of `tag` or `tags` is required", i)
		}
		if td.Tier < 1 {
			return fmt.Errorf("tiers[%d]: tier must be >= 1", i)
		}
		if seen[td.Tier] {
			return fmt.Errorf("tiers[%d]: duplicate tier number %d", i, td.Tier)
		}
		seen[td.Tier] = true
		for j, e := range td.Schedule {
			switch e.Action {
			case "", "wake", "sleep":
			default:
				return fmt.Errorf("tiers[%d].schedule[%d] (%s): action %q not allowed under a tier (use wake, sleep, or empty)", i, j, e.Name, e.Action)
			}
		}
	}
	for i, e := range c.Schedule {
		switch e.Action {
		case "", "night_sleep", "night_wake":
		default:
			return fmt.Errorf("schedule[%d] (%s): action %q not allowed at top level (use night_sleep, night_wake, or empty; per-tier wake/sleep belongs under tiers[].schedule)", i, e.Name, e.Action)
		}
	}
	return nil
}
