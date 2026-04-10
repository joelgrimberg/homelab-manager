# homelab fish client

A thin fish wrapper around the `homelab-manager` Go binary that adds:

- An interactive picker (`gum choose`) when run with no arguments,
  kubens-style.
- Tab completions, including dynamic completion of tier numbers and
  instance VMIDs (queried live from the server).

## Requirements

- `homelab-manager` binary on `$PATH` (install via Homebrew or build from source).
- [`fish`](https://fishshell.com/), [`gum`](https://github.com/charmbracelet/gum),
  `jq`, `curl`.

## Install

### Via Homebrew (recommended)

```fish
brew install joelgrimberg/tap/homelab-manager
```

Fish functions and completions are installed automatically.

### Manual (symlink from repo)

```fish
ln -sf (pwd)/clients/fish/__homelab_server_url.fish  ~/.config/fish/functions/__homelab_server_url.fish
ln -sf (pwd)/clients/fish/homelab.fish               ~/.config/fish/functions/homelab.fish
ln -sf (pwd)/clients/fish/homelab.completions.fish   ~/.config/fish/completions/homelab.fish
```

(Run from the repo root after cloning.)

## Server URL configuration

The server URL is resolved in this order:

1. `$HOMELAB_SERVER` environment variable
2. `~/.config/homelab-manager/config.yaml` (the `server:` key)
3. `http://localhost:8080` (default)

To set up the config file:

```fish
mkdir -p ~/.config/homelab-manager
echo 'server: https://your-server-url' > ~/.config/homelab-manager/config.yaml
```

Or export the env var in your `config.fish`:

```fish
set -gx HOMELAB_SERVER https://your-server-url
```

## Usage

```fish
homelab                           # interactive picker
homelab status                    # show status
homelab wake                      # wake everything
homelab sleep                     # sleep everything
homelab tier wake 1               # wake just tier 1
homelab tier sleep 3              # sleep just tier 3
homelab instance start 240        # start a single VM by VMID
homelab instance stop 117         # stop a single LXC by VMID
```

Tab completion works at every level:

```
homelab <TAB>            → status / wake / sleep / tier / instance
homelab tier <TAB>       → wake / sleep
homelab tier wake <TAB>  → 1  infra   2  local-omni   3  homelab   (live)
homelab instance start <TAB>
                         → 100  vm-a   101  lxc-b   ...            (live)
```
