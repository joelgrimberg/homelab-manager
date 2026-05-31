package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// HandleVAPIDKey returns the server's VAPID public key.
// The PWA needs it to call PushManager.subscribe().
func (pm *PushManager) HandleVAPIDKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"publicKey": pm.VAPIDPublic()})
}

// HandlePushSubscribe accepts a subscription from the PWA and stores it.
func (pm *PushManager) HandlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var sub Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if sub.Endpoint == "" || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		http.Error(w, "missing required subscription fields", http.StatusBadRequest)
		return
	}
	if err := pm.Add(sub); err != nil {
		log.Printf("push subscribe: %v", err)
		http.Error(w, "could not store subscription", http.StatusInternalServerError)
		return
	}
	log.Printf("push: subscription added (%d total)", pm.Count())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "subscribed"})
}

// HandlePushUnsubscribe drops a subscription by endpoint.
func (pm *PushManager) HandlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Endpoint == "" {
		http.Error(w, "missing endpoint", http.StatusBadRequest)
		return
	}
	if err := pm.Remove(body.Endpoint); err != nil {
		log.Printf("push unsubscribe: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "unsubscribed"})
}

// HandlePushTest fires a one-off notification to every stored subscription.
// Body carries a HH:MM:SS timestamp so rapid taps are distinguishable.
func (pm *PushManager) HandlePushTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ts := time.Now().Format("15:04:05")
	go pm.Notify("Homelab", "Test push at "+ts)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "sent", "sent_at": ts})
}
