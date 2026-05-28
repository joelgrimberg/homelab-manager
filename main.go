package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

//go:embed static
var staticFiles embed.FS

const usage = `Usage: homelab-manager <command> [flags]

Commands:
  serve                            Start the web server
  status                           Show current homelab status
  wake                             Wake up the entire homelab
  sleep                            Put the entire homelab to sleep
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
	case "wake":
		cmdTransition("wake", os.Args[2:])
	case "sleep":
		cmdTransition("sleep", os.Args[2:])
	case "tier":
		cmdTier(os.Args[2:])
	case "instance":
		cmdInstance(os.Args[2:])
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

	orch := NewOrchestrator(instances, tierNames, cfg.TierDefs, client)
	go orch.RunRefreshLoop(context.Background(), 60*time.Second)

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("failed to create static filesystem: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/api/status", orch.HandleStatus)
	mux.HandleFunc("/api/wake", orch.HandleWake)
	mux.HandleFunc("/api/sleep", orch.HandleSleep)
	mux.HandleFunc("/api/tier/", orch.HandleTierAction)
	mux.HandleFunc("/api/instance/", orch.HandleInstanceAction)

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

func cmdTransition(action string, args []string) {
	flags := flag.NewFlagSet(action, flag.ExitOnError)
	server := flags.String("server", "", "server URL")
	flags.Parse(args)

	if err := runTransition(ResolveServer(*server), action); err != nil {
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
