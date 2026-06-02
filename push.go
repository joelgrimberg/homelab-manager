package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Subscription is the wire shape PushManager.subscribe() returns from a
// browser, also our on-disk format. The keys.p256dh / keys.auth fields
// are required by webpush-go.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// PushManager holds the live subscription list and persists it to disk.
// Notify is safe to call from any goroutine.
type PushManager struct {
	mu            sync.Mutex
	subs          []Subscription
	path          string
	vapidPublic   string
	vapidPrivate  string
	vapidSubject  string
}

func NewPushManager(path string, cfg WebPushConfig) (*PushManager, error) {
	pm := &PushManager{
		path:         path,
		vapidPublic:  cfg.VAPIDPublic,
		vapidPrivate: cfg.VAPIDPrivate,
		vapidSubject: cfg.VAPIDSubject,
	}
	if err := pm.load(); err != nil {
		return nil, err
	}
	return pm, nil
}

func (pm *PushManager) load() error {
	data, err := os.ReadFile(pm.path)
	if errIsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading subscriptions: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, &pm.subs)
}

func errIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

// persist writes the current subscription list to disk. Caller must hold pm.mu.
func (pm *PushManager) persist() error {
	if err := os.MkdirAll(filepath.Dir(pm.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(pm.subs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(pm.path, data, 0o600)
}

func (pm *PushManager) VAPIDPublic() string {
	return pm.vapidPublic
}

// Add stores a new subscription (idempotent on endpoint).
func (pm *PushManager) Add(sub Subscription) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, s := range pm.subs {
		if s.Endpoint == sub.Endpoint {
			return nil // already subscribed
		}
	}
	pm.subs = append(pm.subs, sub)
	return pm.persist()
}

// Remove drops the subscription matching endpoint.
func (pm *PushManager) Remove(endpoint string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := pm.subs[:0]
	for _, s := range pm.subs {
		if s.Endpoint != endpoint {
			out = append(out, s)
		}
	}
	pm.subs = out
	return pm.persist()
}

func (pm *PushManager) Count() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return len(pm.subs)
}

// NotifyAction is one button on a notification, mirroring the
// NotificationAction wire shape consumed by the service worker. The
// service worker reads `action` in its notificationclick handler.
type NotifyAction struct {
	Action string `json:"action"`
	Title  string `json:"title"`
}

// buildPayload serializes the push body. data + actions are omitted when
// nil so plain Notify calls produce the same bytes as before.
func buildPayload(title, body string, data map[string]any, actions []NotifyAction) []byte {
	p := map[string]any{
		"title": title,
		"body":  body,
	}
	if len(data) > 0 {
		p["data"] = data
	}
	if len(actions) > 0 {
		p["actions"] = actions
	}
	out, _ := json.Marshal(p)
	return out
}

// Notify sends a push to every subscription. Dead subscriptions (404/410)
// are pruned automatically.
func (pm *PushManager) Notify(title, body string) {
	pm.NotifyWithActions(title, body, nil, nil)
}

// NotifyWithActions sends a push that includes a `data` blob (passed to
// the service worker's notificationclick handler) and an `actions` list
// rendered as buttons on the notification.
func (pm *PushManager) NotifyWithActions(title, body string, data map[string]any, actions []NotifyAction) {
	pm.mu.Lock()
	subs := append([]Subscription(nil), pm.subs...)
	pm.mu.Unlock()

	if len(subs) == 0 {
		log.Printf("push: no subscriptions, dropping notify %q", title)
		return
	}

	payload := buildPayload(title, body, data, actions)

	var dead []string
	for _, sub := range subs {
		ws := &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys: webpush.Keys{
				P256dh: sub.Keys.P256dh,
				Auth:   sub.Keys.Auth,
			},
		}
		resp, err := webpush.SendNotification(payload, ws, &webpush.Options{
			Subscriber:      pm.vapidSubject,
			VAPIDPublicKey:  pm.vapidPublic,
			VAPIDPrivateKey: pm.vapidPrivate,
			TTL:             60,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			log.Printf("push: send error to %s: %v", trunc(sub.Endpoint), err)
			continue
		}
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
			log.Printf("push: pruning dead subscription %s (status %d)", trunc(sub.Endpoint), resp.StatusCode)
			dead = append(dead, sub.Endpoint)
			drainAndClose(resp.Body)
		} else if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			log.Printf("push: %s returned %d: %s", trunc(sub.Endpoint), resp.StatusCode, string(body))
		} else {
			drainAndClose(resp.Body)
		}
	}

	if len(dead) > 0 {
		pm.mu.Lock()
		deadSet := map[string]bool{}
		for _, e := range dead {
			deadSet[e] = true
		}
		out := pm.subs[:0]
		for _, s := range pm.subs {
			if !deadSet[s.Endpoint] {
				out = append(out, s)
			}
		}
		pm.subs = out
		_ = pm.persist()
		pm.mu.Unlock()
	}
}

func drainAndClose(b io.ReadCloser) {
	if b == nil {
		return
	}
	_, _ = io.Copy(io.Discard, b)
	_ = b.Close()
}

func trunc(s string) string {
	const max = 60
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

