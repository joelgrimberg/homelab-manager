package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

//go:embed static
var staticFiles embed.FS

const usage = `Usage: homelab-manager <command> [flags]

Commands:
  serve                            Start the web server
  status                           Show current homelab status
  night wake                       Exit night mode (wake non-exempt VMs)
  night sleep                      Enter night mode (sleep non-exempt VMs)
  tier wake <tier>                 Wake a single tier
  tier sleep <tier>                Sleep a single tier
  instance start <vmid>            Start a single instance
  instance stop <vmid>             Stop a single instance

Run 'homelab-manager <command> -h' for command-specific flags.`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "tier":
		cmdTier(os.Args[2:])
	case "instance":
		cmdInstance(os.Args[2:])
	case "night":
		cmdNight(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n%s\n", os.Args[1], usage)
		os.Exit(1)
	}
}

func cmdServe(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := flags.String("config", "config.yaml", "path to config file")
	addr := flags.String("addr", ":8080", "listen address")
	stateDir := flags.String("state-dir", "/var/lib/homelab-manager", "directory for persistent state (subscriptions.json)")
	flags.Parse(args)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	client := NewProxmoxClient(cfg.Proxmox)

	instances, tierNames, err := DiscoverInstances(client, cfg.TierDefs)
	if err != nil {
		log.Fatalf("failed to discover instances: %v", err)
	}
	log.Printf("discovered %d instances across %d tiers", len(instances), len(tierNames))

	orch := NewOrchestrator(instances, tierNames, cfg.TierDefs, cfg.NightMode.KeepAwakeTags, cfg.NeverTouchTags, client)
	go orch.RunRefreshLoop(context.Background(), 60*time.Second)

	pm, err := NewPushManager(filepath.Join(*stateDir, "subscriptions.json"), cfg.WebPush)
	if err != nil {
		log.Fatalf("failed to init push manager: %v", err)
	}
	log.Printf("push: loaded %d subscriptions", pm.Count())

	snooze, err := NewSnoozeManager(filepath.Join(*stateDir, "snooze.json"))
	if err != nil {
		log.Fatalf("failed to init snooze manager: %v", err)
	}

	sched := NewScheduler(orch, pm, snooze)
	if err := sched.Start(cfg.Schedule); err != nil {
		log.Fatalf("scheduler start: %v", err)
	}
	schedHandler := NewScheduleHandler(sched, *configPath)
	snoozeHandler := NewSnoozeHandler(snooze, sched, orch, pm)

	hub := NewEventHub()
	orch.AttachEventHub(hub)
	eventsHandler := NewEventsHandler(hub)

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create static filesystem: %v", err)
	}

	mux := http.NewServeMux()
	// Hero media is served from the state dir (kept out of the embedded
	// static FS because the asset may be SpaceX-copyrighted and the public
	// binary shouldn't ship it). Missing files fall through to 404.
	for _, name := range []string{"hero.mp4", "hero.jpg"} {
		path := filepath.Join(*stateDir, name)
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, path)
		})
	}
	// /countdown serves the SpaceX-style countdown page. The static FS
	// won't auto-resolve /countdown to countdown.html, so wire it
	// explicitly. Falls through to 404 if the file isn't embedded.
	mux.HandleFunc("/countdown", func(w http.ResponseWriter, r *http.Request) {
		f, err := staticFS.Open("countdown.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		stat, _ := f.Stat()
		if ra, ok := f.(io.ReadSeeker); ok && stat != nil {
			http.ServeContent(w, r, "countdown.html", stat.ModTime(), ra)
			return
		}
		io.Copy(w, f)
	})
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		resp := orch.Status()
		if all := snooze.All(); len(all) > 0 {
			resp.Snoozes = all
		}
		if next := sched.NextFires(); len(next) > 0 {
			resp.NextFires = next
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/tier/", orch.HandleTierAction)
	mux.HandleFunc("/api/instance/", orch.HandleInstanceAction)
	mux.HandleFunc("/api/night/", orch.HandleNightAction)
	mux.HandleFunc("/api/push/vapid-key", pm.HandleVAPIDKey)
	mux.HandleFunc("/api/push/subscribe", pm.HandlePushSubscribe)
	mux.HandleFunc("/api/push/unsubscribe", pm.HandlePushUnsubscribe)
	mux.HandleFunc("/api/push/test", pm.HandlePushTest)
	mux.HandleFunc("/api/schedule", schedHandler.Handle)
	mux.HandleFunc("/api/snooze", snoozeHandler.Handle)
	mux.HandleFunc("/api/events", eventsHandler.Handle)

	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func cmdStatus(args []string) {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	server := flags.String("server", "", "server URL")
	flags.Parse(args)

	if err := runStatus(ResolveServer(*server)); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func cmdTier(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: homelab-manager tier <wake|sleep> <tier> [-server URL]")
		os.Exit(1)
	}
	action := args[0]
	if action != "wake" && action != "sleep" {
		fmt.Fprintf(os.Stderr, "tier action must be 'wake' or 'sleep', got %q\n", action)
		os.Exit(1)
	}
	tier, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "tier must be an integer, got %q\n", args[1])
		os.Exit(1)
	}

	flags := flag.NewFlagSet("tier", flag.ExitOnError)
	server := flags.String("server", "", "server URL")
	flags.Parse(args[2:])

	if err := runTier(ResolveServer(*server), action, tier); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func cmdNight(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "Usage: homelab-manager night <wake|sleep> [-server URL]")
		os.Exit(1)
	}
	action := args[0]
	if action != "wake" && action != "sleep" {
		fmt.Fprintf(os.Stderr, "night action must be 'wake' or 'sleep', got %q\n", action)
		os.Exit(1)
	}

	flags := flag.NewFlagSet("night", flag.ExitOnError)
	server := flags.String("server", "", "server URL")
	flags.Parse(args[1:])

	if err := runNight(ResolveServer(*server), action); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func cmdInstance(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: homelab-manager instance <start|stop> <vmid> [-server URL]")
		os.Exit(1)
	}
	action := args[0]
	if action != "start" && action != "stop" {
		fmt.Fprintf(os.Stderr, "instance action must be 'start' or 'stop', got %q\n", action)
		os.Exit(1)
	}
	vmid, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "vmid must be an integer, got %q\n", args[1])
		os.Exit(1)
	}

	flags := flag.NewFlagSet("instance", flag.ExitOnError)
	server := flags.String("server", "", "server URL")
	flags.Parse(args[2:])

	if err := runInstance(ResolveServer(*server), action, vmid); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
