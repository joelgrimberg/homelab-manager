### Tab completions for the `homelab` fish function.
### Install at:  ~/.config/fish/completions/homelab.fish

function __homelab_no_subcommand
    set -l cmd (commandline -opc)
    test (count $cmd) -le 1
end

function __homelab_using_subcommand --argument-names sub
    set -l cmd (commandline -opc)
    test (count $cmd) -ge 2; and test "$cmd[2]" = "$sub"
end

function __homelab_using_subverb --argument-names sub verb
    set -l cmd (commandline -opc)
    test (count $cmd) -ge 3
    and test "$cmd[2]" = "$sub"
    and test "$cmd[3]" = "$verb"
end

# --- Top-level subcommands ---
complete -c homelab -f -n __homelab_no_subcommand -a status   -d "Show current status"
complete -c homelab -f -n __homelab_no_subcommand -a wake     -d "Wake the entire homelab"
complete -c homelab -f -n __homelab_no_subcommand -a sleep    -d "Sleep the entire homelab"
complete -c homelab -f -n __homelab_no_subcommand -a tier     -d "Wake/sleep a single tier"
complete -c homelab -f -n __homelab_no_subcommand -a instance -d "Start/stop a single instance"
complete -c homelab -f -n __homelab_no_subcommand -a help     -d "Show help"

# --- `homelab tier <verb>` ---
complete -c homelab -f -n "__homelab_using_subcommand tier" -a wake  -d "Wake a tier"
complete -c homelab -f -n "__homelab_using_subcommand tier" -a sleep -d "Sleep a tier"

# --- `homelab tier wake <tier>` and `tier sleep <tier>` (dynamic) ---
function __homelab_complete_tiers
    set -l server (__homelab_server_url)
    curl -fsS --max-time 1 $server/api/status 2>/dev/null \
        | jq -r '.tiers[] | "\(.tier)\t\(.name)"' 2>/dev/null
end

complete -c homelab -f -n "__homelab_using_subverb tier wake"  -a "(__homelab_complete_tiers)"
complete -c homelab -f -n "__homelab_using_subverb tier sleep" -a "(__homelab_complete_tiers)"

# --- `homelab instance <verb>` ---
complete -c homelab -f -n "__homelab_using_subcommand instance" -a start -d "Start an instance"
complete -c homelab -f -n "__homelab_using_subcommand instance" -a stop  -d "Stop an instance"

# --- `homelab instance start <vmid>` and `instance stop <vmid>` (dynamic) ---
function __homelab_complete_instances
    set -l server (__homelab_server_url)
    curl -fsS --max-time 1 $server/api/status 2>/dev/null \
        | jq -r '.tiers[] | .instances[] | "\(.vmid)\t\(.name) (\(.status))"' 2>/dev/null
end

complete -c homelab -f -n "__homelab_using_subverb instance start" -a "(__homelab_complete_instances)"
complete -c homelab -f -n "__homelab_using_subverb instance stop"  -a "(__homelab_complete_instances)"
