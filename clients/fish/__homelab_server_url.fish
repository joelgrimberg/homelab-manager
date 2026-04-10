function __homelab_server_url --description "Resolve the homelab-manager server URL"
    # Priority: $HOMELAB_SERVER env > config file > default
    if set -q HOMELAB_SERVER; and test -n "$HOMELAB_SERVER"
        echo $HOMELAB_SERVER
        return
    end

    # Read from ~/.config/homelab-manager/config.yaml (respects XDG_CONFIG_HOME)
    set -l config_dir
    if set -q XDG_CONFIG_HOME; and test -n "$XDG_CONFIG_HOME"
        set config_dir $XDG_CONFIG_HOME
    else
        set config_dir ~/.config
    end

    set -l config_file $config_dir/homelab-manager/config.yaml
    if test -r "$config_file"
        set -l url (string match -r 'server:\s*(.+)' < $config_file)
        if test (count $url) -ge 2; and test -n "$url[2]"
            # Strip surrounding quotes if present
            echo $url[2] | string trim -c '"' | string trim -c "'"
            return
        end
    end

    echo "http://localhost:8080"
end
