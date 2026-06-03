package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// InstanceStatus is the JSON representation of a single instance.
type InstanceStatus struct {
	VMID      int    `json:"vmid"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Protected bool   `json:"protected,omitempty"` // never_touch — UI shows lock, refuses stop
	Stuck     bool   `json:"stuck,omitempty"`     // didn't reach target during last transition
	Source    string `json:"source,omitempty"`    // fallback host name; empty for primary
}

// TierStatus is the JSON representation of a tier group.
type TierStatus struct {
	Tier      int              `json:"tier"`
	Name      string           `json:"name"`
	Instances []InstanceStatus `json:"instances"`
}

// StatusResponse is the JSON response for GET /api/status.
type StatusResponse struct {
	State            string               `json:"state"`
	Transitioning    bool                 `json:"transitioning"`
	Direction        string               `json:"direction,omitempty"`
	CurrentTier      int                  `json:"current_tier,omitempty"`
	NightModeEnabled bool                 `json:"night_mode_enabled"`
	Tiers            []TierStatus         `json:"tiers"`
	Snoozes          map[string]Snooze    `json:"snoozes,omitempty"`
	NextFires        map[string]time.Time `json:"next_fires,omitempty"`
}

// discoveryState is the snapshot of discovered instances and tier names.
// Treated as immutable once stored — Refresh swaps in a brand-new value
// rather than mutating the slice or map, so readers can deref the pointer
// without locking.
type discoveryState struct {
	instances []Instance
	tierNames map[int]string
}

// Orchestrator manages wake/sleep operations and tracks state.
type Orchestrator struct {
	state          atomic.Pointer[discoveryState]
	tierDefs       []TierConfig
	keepAwakeTags  []string // night-mode exempt tags (empty = unconfigured)
	neverTouchTags []string // never_touch — these are never started or stopped
	client         ProxmoxAPI

	// Multi-host: fallback Proxmox sources. The primary is `client`.
	// Instances carrying a non-empty Source are routed via fallbackClients.
	// fallbackSources is used by Refresh to re-discover from all hosts.
	fallbackClients map[string]ProxmoxAPI
	fallbackSources []FallbackSource

	// Inter-tier stabilization delays. Overridable for tests.
	wakeTierDelay  time.Duration
	sleepTierDelay time.Duration
	waitTimeout    time.Duration // per-tier waitForState ceiling (default 240s)
	verifyTimeout  time.Duration // ceiling for verifySubset retry (default 60s)
	pollInterval   time.Duration // status-poll interval inside waitForState (default 3s)

	// stuck holds VMIDs that didn't reach the requested target during the
	// most recent transition (after a retry pass). Replaced wholesale at
	// the start of each transition.
	stuck atomic.Pointer[map[int]bool]

	mu            sync.Mutex
	transitioning bool
	direction     string // "waking", "sleeping", "night-waking", or "night-sleeping"
	currentTier   int    // tier currently being processed

	hub *EventHub // optional; nil disables event emission
}

// AttachEventHub wires in an EventHub so orchestrator transitions are
// streamed over SSE to subscribers (the /countdown page). Calling with
// nil disables emission again. Safe to call once at startup.
func (o *Orchestrator) AttachEventHub(h *EventHub) {
	o.hub = h
}

// emit publishes an event to the attached hub, if any. Cheap no-op when
// the hub is nil — tests can safely ignore the event stream.
func (o *Orchestrator) emit(e Event) {
	if o.hub == nil {
		return
	}
	o.hub.Publish(e)
}

// ProxmoxAPI is the interface for Proxmox operations (allows mocking).
type ProxmoxAPI interface {
	GetStatus(inst Instance) (string, error)
	Start(inst Instance) error
	Stop(inst Instance) error
	ListVMs() ([]ProxmoxInstance, error)
	ListLXCs() ([]ProxmoxInstance, error)
}

func NewOrchestrator(instances []Instance, tierNames map[int]string, tierDefs []TierConfig, keepAwakeTags []string, neverTouchTags []string, client ProxmoxAPI) *Orchestrator {
	o := &Orchestrator{
		tierDefs:         tierDefs,
		keepAwakeTags:    keepAwakeTags,
		neverTouchTags:   neverTouchTags,
		client:           client,
		fallbackClients:  map[string]ProxmoxAPI{},
		wakeTierDelay:    10 * time.Second,
		sleepTierDelay:   5 * time.Second,
		waitTimeout:      240 * time.Second,
		verifyTimeout:    60 * time.Second,
		pollInterval:     3 * time.Second,
	}
	o.state.Store(&discoveryState{instances: instances, tierNames: tierNames})
	empty := map[int]bool{}
	o.stuck.Store(&empty)
	return o
}

// AttachFallback registers a secondary Proxmox source. After this, any
// instance with Source==name in the discovery state will have its
// GetStatus/Start/Stop calls routed to client instead of the primary.
// Also enables periodic Refresh to discover from this source.
func (o *Orchestrator) AttachFallback(src FallbackSource) {
	if o.fallbackClients == nil {
		o.fallbackClients = map[string]ProxmoxAPI{}
	}
	o.fallbackClients[src.Name] = src.Client
	o.fallbackSources = append(o.fallbackSources, src)
}

// clientFor returns the ProxmoxAPI that owns inst — the matching fallback
// client when inst.Source identifies one, else the primary.
func (o *Orchestrator) clientFor(inst Instance) ProxmoxAPI {
	if inst.Source != "" {
		if c, ok := o.fallbackClients[inst.Source]; ok {
			return c
		}
	}
	return o.client
}

// isNeverTouch reports whether the orchestrator must skip this instance.
// True when the instance carries a never_touch tag OR has Protected=true
// (used by fallback-host instances).
func (o *Orchestrator) isNeverTouch(inst Instance) bool {
	if inst.Protected {
		return true
	}
	if len(o.neverTouchTags) == 0 {
		return false
	}
	set := make(map[string]bool, len(o.neverTouchTags))
	for _, t := range o.neverTouchTags {
		set[t] = true
	}
	for _, tag := range inst.Tags {
		if set[tag] {
			return true
		}
	}
	return false
}

// filterTouchable drops never_touch instances from a slice.
func (o *Orchestrator) filterTouchable(instances []Instance) []Instance {
	if len(o.neverTouchTags) == 0 {
		return instances
	}
	out := make([]Instance, 0, len(instances))
	for _, inst := range instances {
		if !o.isNeverTouch(inst) {
			out = append(out, inst)
		}
	}
	return out
}

// Refresh re-discovers instances from Proxmox and atomically swaps in the
// new state. Skipped if a transition is in progress, to avoid mutating the
// instance list while doWake/doSleep is iterating over it.
func (o *Orchestrator) Refresh() error {
	o.mu.Lock()
	inTransit := o.transitioning
	o.mu.Unlock()
	if inTransit {
		return nil
	}

	instances, tierNames, err := DiscoverInstances(o.client, o.fallbackSources, o.tierDefs)
	if err != nil {
		return err
	}
	o.state.Store(&discoveryState{instances: instances, tierNames: tierNames})
	return nil
}

// RunRefreshLoop calls Refresh on a fixed interval until ctx is cancelled.
// Intended to run in its own goroutine.
func (o *Orchestrator) RunRefreshLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := o.Refresh(); err != nil {
				log.Printf("discovery refresh failed: %v", err)
			}
		}
	}
}

func (o *Orchestrator) isTransitioning() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.transitioning
}

// tiers returns sorted unique tier numbers.
func (o *Orchestrator) tiers() []int {
	s := o.state.Load()
	seen := map[int]bool{}
	for _, inst := range s.instances {
		seen[inst.Tier] = true
	}
	tiers := make([]int, 0, len(seen))
	for t := range seen {
		tiers = append(tiers, t)
	}
	sort.Ints(tiers)
	return tiers
}

// findInstance returns the instance with the given VMID, or nil if not found.
func (o *Orchestrator) findInstance(vmid int) *Instance {
	s := o.state.Load()
	for i := range s.instances {
		if s.instances[i].VMID == vmid {
			return &s.instances[i]
		}
	}
	return nil
}

// instancesByTier returns instances for a given tier.
func (o *Orchestrator) instancesByTier(tier int) []Instance {
	s := o.state.Load()
	var result []Instance
	for _, inst := range s.instances {
		if inst.Tier == tier {
			result = append(result, inst)
		}
	}
	return result
}

// computeState determines the overall homelab state from instance statuses.
// exemptVMIDs marks instances exempt from night-mode sleep — if every
// non-exempt instance is stopped while exempt ones are running, state is "night".
func computeState(tiers []TierStatus, transitioning bool, exemptVMIDs map[int]bool) string {
	if transitioning {
		return "transitioning"
	}

	allRunning := true
	allStopped := true
	nonExemptAllStopped := true
	exemptAnyRunning := false
	haveNonExempt := false
	haveExempt := false

	for _, tier := range tiers {
		for _, inst := range tier.Instances {
			if inst.Status != "running" {
				allRunning = false
			}
			if inst.Status != "stopped" {
				allStopped = false
			}
			if exemptVMIDs[inst.VMID] {
				haveExempt = true
				if inst.Status == "running" {
					exemptAnyRunning = true
				}
			} else {
				haveNonExempt = true
				if inst.Status != "stopped" {
					nonExemptAllStopped = false
				}
			}
		}
	}

	if allRunning {
		return "awake"
	}
	if allStopped {
		return "asleep"
	}
	if haveExempt && haveNonExempt && nonExemptAllStopped && exemptAnyRunning {
		return "night"
	}
	return "mixed"
}

// Status queries all instances and returns the current state.
func (o *Orchestrator) Status() StatusResponse {
	s := o.state.Load()
	tiers := o.tiers()
	tierStatuses := make([]TierStatus, 0, len(tiers))

	stuckSet := *o.stuck.Load()

	for _, tier := range tiers {
		instances := o.instancesByTier(tier)
		statuses := make([]InstanceStatus, 0, len(instances))

		for _, inst := range instances {
			status, err := o.clientFor(inst).GetStatus(inst)
			if err != nil {
				log.Printf("error getting status for %s (%d): %v", inst.Name, inst.VMID, err)
				status = "unknown"
			}
			statuses = append(statuses, InstanceStatus{
				VMID:      inst.VMID,
				Name:      inst.Name,
				Type:      inst.Type,
				Status:    status,
				Protected: o.isNeverTouch(inst),
				Stuck:     stuckSet[inst.VMID],
				Source:    inst.Source,
			})
		}

		name := s.tierNames[tier]
		if name == "" {
			name = "unnamed"
		}

		tierStatuses = append(tierStatuses, TierStatus{
			Tier:      tier,
			Name:      name,
			Instances: statuses,
		})
	}

	o.mu.Lock()
	transitioning := o.transitioning
	direction := o.direction
	currentTier := o.currentTier
	o.mu.Unlock()

	exemptVMIDs := o.exemptVMIDs()

	return StatusResponse{
		State:            computeState(tierStatuses, transitioning, exemptVMIDs),
		Transitioning:    transitioning,
		Direction:        direction,
		CurrentTier:      currentTier,
		NightModeEnabled: len(o.keepAwakeTags) > 0,
		Tiers:            tierStatuses,
	}
}

// exemptVMIDs returns the set of VMIDs that are exempt from night-mode sleep.
// Returns nil if night mode is unconfigured.
func (o *Orchestrator) exemptVMIDs() map[int]bool {
	if len(o.keepAwakeTags) == 0 {
		return nil
	}
	keep := make(map[string]bool, len(o.keepAwakeTags))
	for _, t := range o.keepAwakeTags {
		keep[t] = true
	}
	s := o.state.Load()
	result := map[int]bool{}
	for _, inst := range s.instances {
		for _, tag := range inst.Tags {
			if keep[tag] {
				result[inst.VMID] = true
				break
			}
		}
	}
	return result
}

// WakeTier starts all instances in a single tier.
// Returns false if a transition is already in progress, or if the tier
// has no instances (unknown tier).
func (o *Orchestrator) WakeTier(tier int) (started bool, unknown bool) {
	instances := o.instancesByTier(tier)
	if len(instances) == 0 {
		return false, true
	}

	o.mu.Lock()
	if o.transitioning {
		o.mu.Unlock()
		return false, false
	}
	o.transitioning = true
	o.direction = "waking"
	o.currentTier = tier
	o.mu.Unlock()

	go o.doTierTransition(tier, instances, "start")
	return true, false
}

// SleepTier stops all instances in a single tier.
func (o *Orchestrator) SleepTier(tier int) (started bool, unknown bool) {
	instances := o.instancesByTier(tier)
	if len(instances) == 0 {
		return false, true
	}

	o.mu.Lock()
	if o.transitioning {
		o.mu.Unlock()
		return false, false
	}
	o.transitioning = true
	o.direction = "sleeping"
	o.currentTier = tier
	o.mu.Unlock()

	go o.doTierTransition(tier, instances, "stop")
	return true, false
}

// partitionByExemption splits the discovered instances into (exempt, nonExempt)
// where exempt instances carry at least one keep-awake tag. never_touch
// instances are excluded from BOTH lists (we never start or stop them).
// Returns (nil, nil) if night mode is unconfigured.
func (o *Orchestrator) partitionByExemption() (exempt, nonExempt []Instance) {
	if len(o.keepAwakeTags) == 0 {
		return nil, nil
	}
	keep := make(map[string]bool, len(o.keepAwakeTags))
	for _, t := range o.keepAwakeTags {
		keep[t] = true
	}
	s := o.state.Load()
	for _, inst := range s.instances {
		if o.isNeverTouch(inst) {
			continue
		}
		isExempt := false
		for _, tag := range inst.Tags {
			if keep[tag] {
				isExempt = true
				break
			}
		}
		if isExempt {
			exempt = append(exempt, inst)
		} else {
			nonExempt = append(nonExempt, inst)
		}
	}
	return exempt, nonExempt
}

// NightSleep brings the homelab into night state: ensures every keep-awake
// instance is running, then stops the rest. Idempotent regardless of starting
// state (works whether we came from awake, asleep, or mixed).
func (o *Orchestrator) NightSleep() (started bool, unconfigured bool) {
	if len(o.keepAwakeTags) == 0 {
		return false, true
	}

	o.mu.Lock()
	if o.transitioning {
		o.mu.Unlock()
		return false, false
	}
	o.transitioning = true
	o.direction = "night-sleeping"
	o.mu.Unlock()

	go o.doNightSleep()
	return true, false
}

// NightWake exits night state by starting the non-exempt instances. Exempt
// instances are already up; calling Wake() instead would also work but does
// extra no-op starts.
func (o *Orchestrator) NightWake() (started bool, unconfigured bool) {
	if len(o.keepAwakeTags) == 0 {
		return false, true
	}

	o.mu.Lock()
	if o.transitioning {
		o.mu.Unlock()
		return false, false
	}
	o.transitioning = true
	o.direction = "night-waking"
	o.mu.Unlock()

	go o.doNightWake()
	return true, false
}

func (o *Orchestrator) doNightSleep() {
	defer o.endTransition()
	o.clearStuck()

	o.emit(Event{Type: "sleep_start"})

	exempt, nonExempt := o.partitionByExemption()

	// Bring exempt up first so dns stays resolvable while we shut the rest down.
	o.processSubset(exempt, "start")
	o.processSubset(nonExempt, "stop")

	o.recordStuck(o.verifySubset(exempt, "running"))
	o.recordStuck(o.verifySubset(nonExempt, "stopped"))

	o.emit(Event{Type: "sleep_complete"})

	log.Println("night sleep complete")
}

func (o *Orchestrator) doNightWake() {
	defer o.endTransition()
	o.clearStuck()

	_, nonExempt := o.partitionByExemption()
	o.processSubset(nonExempt, "start")

	o.recordStuck(o.verifySubset(nonExempt, "running"))

	log.Println("night wake complete")
}

func (o *Orchestrator) endTransition() {
	o.mu.Lock()
	o.transitioning = false
	o.direction = ""
	o.currentTier = 0
	o.mu.Unlock()
}

// processSubset runs start or stop on a subset of instances, grouped by tier
// and processed in dependency order (ascending for start, descending for stop).
// Does NOT manage the transitioning flag — caller does that. No-op on empty.
func (o *Orchestrator) processSubset(instances []Instance, action string) {
	if len(instances) == 0 {
		return
	}

	byTier := map[int][]Instance{}
	for _, inst := range instances {
		byTier[inst.Tier] = append(byTier[inst.Tier], inst)
	}
	tiers := make([]int, 0, len(byTier))
	for t := range byTier {
		tiers = append(tiers, t)
	}
	sort.Ints(tiers)
	if action == "stop" {
		for i, j := 0, len(tiers)-1; i < j; i, j = i+1, j-1 {
			tiers[i], tiers[j] = tiers[j], tiers[i]
		}
	}

	target := "running"
	delay := o.wakeTierDelay
	if action == "stop" {
		target = "stopped"
		delay = o.sleepTierDelay
	}

	for i, tier := range tiers {
		o.mu.Lock()
		o.currentTier = tier
		o.mu.Unlock()

		tierInsts := byTier[tier]
		log.Printf("night %s tier %d: %d instances", action, tier, len(tierInsts))

		for _, inst := range tierInsts {
			o.emit(Event{
				Type: "vm_action", Action: action,
				VMID: inst.VMID, Name: inst.Name, Tier: inst.Tier,
			})
			var err error
			if action == "start" {
				err = o.clientFor(inst).Start(inst)
			} else {
				err = o.clientFor(inst).Stop(inst)
			}
			if err != nil {
				log.Printf("error %sing %s (%d): %v", action, inst.Name, inst.VMID, err)
				o.emit(Event{
					Type: "vm_error",
					VMID: inst.VMID, Name: inst.Name, Tier: inst.Tier,
					Error: err.Error(),
				})
			}
		}

		o.waitForState(tierInsts, target)

		if i < len(tiers)-1 {
			time.Sleep(delay)
		}
	}
}

// doTierTransition runs a single-tier start or stop in the background.
// never_touch instances in the tier are skipped.
func (o *Orchestrator) doTierTransition(tier int, instances []Instance, action string) {
	defer func() {
		o.mu.Lock()
		o.transitioning = false
		o.direction = ""
		o.currentTier = 0
		o.mu.Unlock()
	}()
	o.clearStuck()

	instances = o.filterTouchable(instances)

	target := "running"
	verb := "waking"
	if action == "stop" {
		target = "stopped"
		verb = "sleeping"
	}

	log.Printf("%s tier %d (%s): %d instances", verb, tier, o.state.Load().tierNames[tier], len(instances))
	for _, inst := range instances {
		var err error
		if action == "start" {
			err = o.clientFor(inst).Start(inst)
		} else {
			err = o.clientFor(inst).Stop(inst)
		}
		if err != nil {
			log.Printf("error %s %s (%d): %v", verb, inst.Name, inst.VMID, err)
		}
	}
	o.waitForState(instances, target)
	o.recordStuck(o.verifySubset(instances, target))
	log.Printf("tier %d %s complete", tier, action)
}

func (o *Orchestrator) waitForState(instances []Instance, target string) {
	o.waitForStateWithTimeout(instances, target, o.waitTimeout)
}

func (o *Orchestrator) waitForStateWithTimeout(instances []Instance, target string, timeout time.Duration) {
	poll := o.pollInterval
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		allReady := true
		for _, inst := range instances {
			status, err := o.clientFor(inst).GetStatus(inst)
			if err != nil {
				allReady = false
				continue
			}
			if status != target {
				allReady = false
			}
		}
		if allReady {
			return
		}
		time.Sleep(poll)
	}

	log.Printf("timeout waiting for instances to reach %q", target)
}

// verifySubset re-issues the start/stop action for any instance whose
// current Proxmox state doesn't match target, then waits up to
// verifyTimeout for them to settle. Returns the VMIDs still not at target.
func (o *Orchestrator) verifySubset(instances []Instance, target string) []int {
	if len(instances) == 0 {
		return nil
	}
	var laggards []Instance
	for _, inst := range instances {
		status, err := o.clientFor(inst).GetStatus(inst)
		if err != nil {
			log.Printf("verify: error getting status for %s (%d): %v", inst.Name, inst.VMID, err)
			laggards = append(laggards, inst)
			continue
		}
		if status != target {
			laggards = append(laggards, inst)
		}
	}
	if len(laggards) == 0 {
		return nil
	}

	verb := "starting"
	if target == "stopped" {
		verb = "stopping"
	}
	for _, inst := range laggards {
		log.Printf("verify: re-%s %s (%d)", verb, inst.Name, inst.VMID)
		var err error
		if target == "running" {
			err = o.clientFor(inst).Start(inst)
		} else {
			err = o.clientFor(inst).Stop(inst)
		}
		if err != nil {
			log.Printf("verify: error %s %s (%d): %v", verb, inst.Name, inst.VMID, err)
		}
	}

	o.waitForStateWithTimeout(laggards, target, o.verifyTimeout)

	var stuck []int
	for _, inst := range laggards {
		status, err := o.clientFor(inst).GetStatus(inst)
		if err != nil || status != target {
			stuck = append(stuck, inst.VMID)
			log.Printf("verify: %s (%d) still not %s", inst.Name, inst.VMID, target)
		}
	}
	return stuck
}

// clearStuck resets the stuck map at the start of a transition.
func (o *Orchestrator) clearStuck() {
	empty := map[int]bool{}
	o.stuck.Store(&empty)
}

// recordStuck stores the union of given stuck VMIDs.
func (o *Orchestrator) recordStuck(vmids []int) {
	if len(vmids) == 0 {
		return
	}
	cur := *o.stuck.Load()
	next := make(map[int]bool, len(cur)+len(vmids))
	for k, v := range cur {
		next[k] = v
	}
	for _, id := range vmids {
		next[id] = true
	}
	o.stuck.Store(&next)
}

// HandleStatus handles GET /api/status.
func (o *Orchestrator) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := o.Status()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleTierAction handles POST /api/tier/{tier}/wake and /sleep.
func (o *Orchestrator) HandleTierAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse /api/tier/{tier}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/tier/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid path, expected /api/tier/{tier}/{action}", http.StatusBadRequest)
		return
	}

	tier, err := strconv.Atoi(parts[0])
	if err != nil {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}
	action := parts[1]

	if action != "wake" && action != "sleep" {
		http.Error(w, "action must be 'wake' or 'sleep'", http.StatusBadRequest)
		return
	}

	var started, unknown bool
	if action == "wake" {
		started, unknown = o.WakeTier(tier)
	} else {
		started, unknown = o.SleepTier(tier)
	}

	w.Header().Set("Content-Type", "application/json")
	if unknown {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "tier not found"})
		return
	}
	if !started {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "transition already in progress"})
		return
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// HandleNightAction handles POST /api/night/wake and /api/night/sleep.
func (o *Orchestrator) HandleNightAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	action := strings.TrimPrefix(r.URL.Path, "/api/night/")
	if action != "wake" && action != "sleep" {
		http.Error(w, "action must be 'wake' or 'sleep'", http.StatusBadRequest)
		return
	}

	var started, unconfigured bool
	if action == "wake" {
		started, unconfigured = o.NightWake()
	} else {
		started, unconfigured = o.NightSleep()
	}

	w.Header().Set("Content-Type", "application/json")
	if unconfigured {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "night mode is not configured"})
		return
	}
	if !started {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "transition already in progress"})
		return
	}
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// HandleInstanceAction handles POST /api/instance/{vmid}/start and /stop.
func (o *Orchestrator) HandleInstanceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse /api/instance/{vmid}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/instance/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid path, expected /api/instance/{vmid}/{action}", http.StatusBadRequest)
		return
	}

	vmid, err := strconv.Atoi(parts[0])
	if err != nil {
		http.Error(w, "invalid vmid", http.StatusBadRequest)
		return
	}
	action := parts[1]

	if action != "start" && action != "stop" {
		http.Error(w, "action must be 'start' or 'stop'", http.StatusBadRequest)
		return
	}

	inst := o.findInstance(vmid)
	if inst == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "instance not found"})
		return
	}

	if o.isNeverTouch(*inst) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "instance is protected (never_touch) — refusing"})
		return
	}

	if o.isTransitioning() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "transition already in progress"})
		return
	}

	if action == "start" {
		err = o.clientFor(*inst).Start(*inst)
	} else {
		err = o.clientFor(*inst).Stop(*inst)
	}

	if err != nil {
		log.Printf("error %sing %s (%d): %v", action, inst.Name, inst.VMID, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	log.Printf("instance %s (%d): %s", inst.Name, inst.VMID, action)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": action + "ed"})
}
