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
	VMID   int    `json:"vmid"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// TierStatus is the JSON representation of a tier group.
type TierStatus struct {
	Tier      int              `json:"tier"`
	Name      string           `json:"name"`
	Instances []InstanceStatus `json:"instances"`
}

// StatusResponse is the JSON response for GET /api/status.
type StatusResponse struct {
	State         string       `json:"state"`
	Transitioning bool         `json:"transitioning"`
	Direction     string       `json:"direction,omitempty"`
	CurrentTier   int          `json:"current_tier,omitempty"`
	Tiers         []TierStatus `json:"tiers"`
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
	state    atomic.Pointer[discoveryState]
	tierDefs []TierConfig
	client   ProxmoxAPI

	mu            sync.Mutex
	transitioning bool
	direction     string // "waking" or "sleeping"
	currentTier   int    // tier currently being processed
}

// ProxmoxAPI is the interface for Proxmox operations (allows mocking).
type ProxmoxAPI interface {
	GetStatus(inst Instance) (string, error)
	Start(inst Instance) error
	Stop(inst Instance) error
	ListVMs() ([]ProxmoxInstance, error)
	ListLXCs() ([]ProxmoxInstance, error)
}

func NewOrchestrator(instances []Instance, tierNames map[int]string, tierDefs []TierConfig, client ProxmoxAPI) *Orchestrator {
	o := &Orchestrator{
		tierDefs: tierDefs,
		client:   client,
	}
	o.state.Store(&discoveryState{instances: instances, tierNames: tierNames})
	return o
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

	instances, tierNames, err := DiscoverInstances(o.client, o.tierDefs)
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
func computeState(tiers []TierStatus, transitioning bool) string {
	if transitioning {
		return "transitioning"
	}

	allRunning := true
	allStopped := true

	for _, tier := range tiers {
		for _, inst := range tier.Instances {
			if inst.Status != "running" {
				allRunning = false
			}
			if inst.Status != "stopped" {
				allStopped = false
			}
		}
	}

	if allRunning {
		return "awake"
	}
	if allStopped {
		return "asleep"
	}
	return "mixed"
}

// Status queries all instances and returns the current state.
func (o *Orchestrator) Status() StatusResponse {
	s := o.state.Load()
	tiers := o.tiers()
	tierStatuses := make([]TierStatus, 0, len(tiers))

	for _, tier := range tiers {
		instances := o.instancesByTier(tier)
		statuses := make([]InstanceStatus, 0, len(instances))

		for _, inst := range instances {
			status, err := o.client.GetStatus(inst)
			if err != nil {
				log.Printf("error getting status for %s (%d): %v", inst.Name, inst.VMID, err)
				status = "unknown"
			}
			statuses = append(statuses, InstanceStatus{
				VMID:   inst.VMID,
				Name:   inst.Name,
				Type:   inst.Type,
				Status: status,
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

	return StatusResponse{
		State:         computeState(tierStatuses, transitioning),
		Transitioning: transitioning,
		Direction:     direction,
		CurrentTier:   currentTier,
		Tiers:         tierStatuses,
	}
}

// Wake starts all instances tier by tier (1→4).
func (o *Orchestrator) Wake() bool {
	o.mu.Lock()
	if o.transitioning {
		o.mu.Unlock()
		return false
	}
	o.transitioning = true
	o.direction = "waking"
	o.mu.Unlock()

	go o.doWake()
	return true
}

// Sleep stops all instances tier by tier (4→1).
func (o *Orchestrator) Sleep() bool {
	o.mu.Lock()
	if o.transitioning {
		o.mu.Unlock()
		return false
	}
	o.transitioning = true
	o.direction = "sleeping"
	o.mu.Unlock()

	go o.doSleep()
	return true
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

// doTierTransition runs a single-tier start or stop in the background.
func (o *Orchestrator) doTierTransition(tier int, instances []Instance, action string) {
	defer func() {
		o.mu.Lock()
		o.transitioning = false
		o.direction = ""
		o.currentTier = 0
		o.mu.Unlock()
	}()

	if action == "start" {
		log.Printf("waking tier %d (%s): %d instances", tier, o.state.Load().tierNames[tier], len(instances))
		for _, inst := range instances {
			if err := o.client.Start(inst); err != nil {
				log.Printf("error starting %s (%d): %v", inst.Name, inst.VMID, err)
			}
		}
		o.waitForState(instances, "running")
		log.Printf("tier %d wake complete", tier)
		return
	}

	log.Printf("sleeping tier %d (%s): %d instances", tier, o.state.Load().tierNames[tier], len(instances))
	for _, inst := range instances {
		if err := o.client.Stop(inst); err != nil {
			log.Printf("error stopping %s (%d): %v", inst.Name, inst.VMID, err)
		}
	}
	o.waitForState(instances, "stopped")
	log.Printf("tier %d sleep complete", tier)
}

func (o *Orchestrator) doWake() {
	defer func() {
		o.mu.Lock()
		o.transitioning = false
		o.direction = ""
		o.currentTier = 0
		o.mu.Unlock()
	}()

	tiers := o.tiers()
	for _, tier := range tiers {
		o.mu.Lock()
		o.currentTier = tier
		o.mu.Unlock()

		instances := o.instancesByTier(tier)
		log.Printf("waking tier %d (%s): %d instances", tier, o.state.Load().tierNames[tier], len(instances))

		// Fire all start commands in this tier
		for _, inst := range instances {
			if err := o.client.Start(inst); err != nil {
				log.Printf("error starting %s (%d): %v", inst.Name, inst.VMID, err)
			}
		}

		// Wait for all to reach "running"
		o.waitForState(instances, "running")

		// Stabilization delay — let services inside VMs actually start
		// before proceeding to dependent tiers
		if tier != tiers[len(tiers)-1] {
			time.Sleep(10 * time.Second)
		}
	}

	log.Println("wake complete")
}

func (o *Orchestrator) doSleep() {
	defer func() {
		o.mu.Lock()
		o.transitioning = false
		o.direction = ""
		o.currentTier = 0
		o.mu.Unlock()
	}()

	tiers := o.tiers()
	// Reverse order for sleep
	for i := len(tiers) - 1; i >= 0; i-- {
		tier := tiers[i]
		o.mu.Lock()
		o.currentTier = tier
		o.mu.Unlock()

		instances := o.instancesByTier(tier)
		log.Printf("sleeping tier %d (%s): %d instances", tier, o.state.Load().tierNames[tier], len(instances))

		// Fire all stop commands in this tier
		for _, inst := range instances {
			if err := o.client.Stop(inst); err != nil {
				log.Printf("error stopping %s (%d): %v", inst.Name, inst.VMID, err)
			}
		}

		// Wait for all to reach "stopped"
		o.waitForState(instances, "stopped")

		// Brief delay between tiers
		if i > 0 {
			time.Sleep(5 * time.Second)
		}
	}

	log.Println("sleep complete")
}

func (o *Orchestrator) waitForState(instances []Instance, target string) {
	timeout := 120 * time.Second
	poll := 3 * time.Second
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		allReady := true
		for _, inst := range instances {
			status, err := o.client.GetStatus(inst)
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

// HandleWake handles POST /api/wake.
func (o *Orchestrator) HandleWake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !o.Wake() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "transition already in progress"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
}

// HandleSleep handles POST /api/sleep.
func (o *Orchestrator) HandleSleep(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if !o.Sleep() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "transition already in progress"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "started"})
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

	if o.isTransitioning() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "transition already in progress"})
		return
	}

	if action == "start" {
		err = o.client.Start(*inst)
	} else {
		err = o.client.Stop(*inst)
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
