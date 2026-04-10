function homelab --description "Manage the Proxmox homelab (interactive picker + dispatcher)"
    set -l server (__homelab_server_url)

    # Subcommand dispatcher — forwards to the homelab-manager Go binary.
    if test (count $argv) -gt 0
        switch $argv[1]
            case status wake sleep
                command homelab-manager $argv[1] -server $server
                return $status
            case tier
                if test (count $argv) -lt 3
                    echo "usage: homelab tier <wake|sleep> <tier>" >&2
                    return 2
                end
                command homelab-manager tier $argv[2] $argv[3] -server $server
                return $status
            case instance
                if test (count $argv) -lt 3
                    echo "usage: homelab instance <start|stop> <vmid>" >&2
                    return 2
                end
                command homelab-manager instance $argv[2] $argv[3] -server $server
                return $status
            case help -h --help
                __homelab_help
                return 0
            case '*'
                echo "homelab: unknown subcommand '$argv[1]'" >&2
                __homelab_help >&2
                return 2
        end
    end

    # No args → interactive picker (kubens-style).
    set -l choice (printf '%s\n' \
        "status               Show current homelab status" \
        "wake                 Wake the entire homelab" \
        "sleep                Sleep the entire homelab" \
        "tier wake…           Pick a tier to wake" \
        "tier sleep…          Pick a tier to sleep" \
        "instance start…      Pick an instance to start" \
        "instance stop…       Pick an instance to stop" \
        | gum choose --header "homelab")

    test -z "$choice"; and return 130

    set -l action (string split -m1 ' ' -- $choice)[1]
    switch $action
        case status
            homelab status
        case wake
            homelab wake
        case sleep
            homelab sleep
        case tier
            set -l verb (string split ' ' -- $choice)[2]
            set -l verb (string trim -c '…' -- $verb)
            set -l tier (__homelab_pick_tier $server)
            test -z "$tier"; and return 130
            homelab tier $verb $tier
        case instance
            set -l verb (string split ' ' -- $choice)[2]
            set -l verb (string trim -c '…' -- $verb)
            set -l vmid (__homelab_pick_instance $server)
            test -z "$vmid"; and return 130
            homelab instance $verb $vmid
    end
end

function __homelab_help
    echo "Usage: homelab [<command>] [<args>]"
    echo
    echo "Commands:"
    echo "  status                          Show current homelab status"
    echo "  wake | sleep                    Wake or sleep the entire homelab"
    echo "  tier <wake|sleep> <tier>        Wake or sleep a single tier"
    echo "  instance <start|stop> <vmid>    Start or stop a single instance"
    echo
    echo "Run with no arguments for an interactive picker."
end

# Fetch /api/status and pipe a one-line-per-tier list into gum choose,
# echoing just the picked tier number.
function __homelab_pick_tier --argument-names server
    set -l json (curl -fsS $server/api/status 2>/dev/null)
    test -z "$json"; and echo "homelab: failed to fetch status from $server" >&2; and return 1

    set -l line (echo $json | jq -r '.tiers[] | "\(.tier)\t\(.name)\t(\(.instances | length) instances)"' \
        | gum choose --header "Pick a tier")
    test -z "$line"; and return 1
    echo $line | cut -f1
end

# Fetch /api/status and pipe a one-line-per-instance list into gum choose,
# echoing just the picked vmid.
function __homelab_pick_instance --argument-names server
    set -l json (curl -fsS $server/api/status 2>/dev/null)
    test -z "$json"; and echo "homelab: failed to fetch status from $server" >&2; and return 1

    set -l line (echo $json | jq -r '.tiers[] | .tier as $t | .instances[] | "\(.vmid)\t\(.name)\t\(.status)\ttier \($t)"' \
        | gum choose --header "Pick an instance")
    test -z "$line"; and return 1
    echo $line | cut -f1
end
