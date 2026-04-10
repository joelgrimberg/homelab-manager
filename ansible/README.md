# Homelab Ansible

Automated OS updates for the non-Kubernetes tier-1 "infra" VMs/LXCs on Proxmox.

## What it does

- Dynamically discovers `infra`-tagged running instances via the Proxmox API
- Updates Debian/Ubuntu hosts with `apt dist-upgrade` (+ autoremove/autoclean)
- Updates Alpine hosts with `apk upgrade`
- Reboots VMs only when needed (reboot-required file on Debian, kernel diff on Alpine)
- Handles `dns-secondary` first, solo, with pre/post DNS health checks

## Hosts managed

Discovered dynamically — the list comes from whichever instances have the
Proxmox `infra` tag and are running. Run `ansible-inventory --graph` to see
the current set.

## Prerequisites (one-time)

1. **Install Ansible + collections**
   ```bash
   pip install ansible netaddr
   cd ansible
   ansible-galaxy collection install -r requirements.yml
   ```

2. **Tag infra VMs/LXCs in Proxmox** so the dynamic inventory picks them up:
   ```bash
   ssh pve
   pvesh set /nodes/pve/qemu/<VMID>/config --tags "infra"
   pvesh set /nodes/pve/lxc/<VMID>/config --tags "infra"
   ```

3. **SSH key to each host**
   ```bash
   for h in dns-secondary openbao runner omni tsidp monitoring pulse certmgr; do
     ssh-copy-id root@$h
   done
   ```

4. **Python on Alpine LXCs** (Ansible needs an interpreter)
   ```bash
   apk add python3
   ```

## Usage

```bash
cd ansible/
export PROXMOX_TOKEN_SECRET="..."

# Verify inventory discovers the right hosts
ansible-inventory --graph

# Dry run
ansible-playbook playbooks/update.yml --check --diff

# Full update
ansible-playbook playbooks/update.yml

# Single host
ansible-playbook playbooks/update.yml --limit monitoring
```

## Layout

```
ansible/
├── ansible.cfg
├── requirements.yml
├── inventory/
│   ├── homelab.proxmox.yml       # Dynamic Proxmox inventory
│   └── group_vars/all.yml        # SSH, become, timeouts
├── playbooks/
│   └── update.yml                # DNS-first, then the rest
├── roles/
│   └── update/
│       └── tasks/
│           ├── main.yml          # OS dispatcher
│           ├── debian.yml
│           └── alpine.yml
└── cron/
    ├── ansible-infra-update.sh        # Wrapper installed on mgmt-host
    ├── ansible-infra-update.cron      # /etc/cron.d entry (Sunday 04:00 UTC)
    └── ansible-infra-update.logrotate # /etc/logrotate.d entry
```

## Scheduled run on `mgmt-host`

The playbook is run weekly on the always-on `mgmt-host` LXC by cron.
The wrapper, cron entry and logrotate config in `cron/` are the source of
truth — install them on a fresh `mgmt-host` like this:

```bash
# 1. Bump memory if it's still 256 MB (ansible needs ~512 MB during a run)
ssh pve "pct set <VMID> --memory 512"

# 2. Install ansible + deps inside the LXC
ssh pve "pct exec <VMID> -- apt-get install -y ansible python3-requests dnsutils locales"
ssh pve "pct exec <VMID> -- sh -c 'sed -i s/#.en_US.UTF-8/en_US.UTF-8/ /etc/locale.gen && locale-gen && update-locale LANG=en_US.UTF-8'"
ssh pve "pct exec <VMID> -- ansible-galaxy collection install community.proxmox"

# 3. Generate an SSH key on mgmt-host and deploy the pubkey to all 8 hosts.
#    IMPORTANT: when appending to authorized_keys, ensure the existing file
#    ends with a newline first, otherwise you can corrupt both keys. The
#    safe pattern is: printf '\n%s\n' "$KEY" >> ~/.ssh/authorized_keys
ssh pve "pct exec <VMID> -- ssh-keygen -t ed25519 -N '' -f /root/.ssh/id_ed25519"

# 4. Copy the ansible tree into /opt/homelab-manager/ansible/ on the LXC.
#    From a checkout of this repo:
tar -czf /tmp/homelab-ansible.tar.gz ansible/
scp /tmp/homelab-ansible.tar.gz pve:/tmp/
ssh pve "pct push <VMID> /tmp/homelab-ansible.tar.gz /tmp/homelab-ansible.tar.gz \
  && pct exec <VMID> -- sh -c 'mkdir -p /opt/homelab-manager && tar -xzf /tmp/homelab-ansible.tar.gz -C /opt/homelab-manager/ && chown -R root:root /opt/homelab-manager && rm /tmp/homelab-ansible.tar.gz'"

# 5. Store the Proxmox token (chmod 600, root-owned)
ssh pve "pct exec <VMID> -- sh -c 'umask 077 && mkdir -p /etc/homelab-manager && cat > /etc/homelab-manager/ansible.env <<EOF
PROXMOX_TOKEN_SECRET=<paste secret here>
EOF'"

# 6. Install the wrapper, cron entry and logrotate config from cron/
ssh pve "pct push <VMID> /opt/homelab-manager/ansible/cron/ansible-infra-update.sh        /usr/local/bin/ansible-infra-update.sh"
ssh pve "pct exec <VMID> -- chmod 755 /usr/local/bin/ansible-infra-update.sh"
ssh pve "pct push <VMID> /opt/homelab-manager/ansible/cron/ansible-infra-update.cron      /etc/cron.d/ansible-infra-update"
ssh pve "pct push <VMID> /opt/homelab-manager/ansible/cron/ansible-infra-update.logrotate /etc/logrotate.d/ansible-infra-update"

# 7. Smoke test
ssh pve "pct exec <VMID> -- /usr/local/bin/ansible-infra-update.sh && tail /var/log/ansible-infra-update.log"
```

Cron runs as `root` on `mgmt-host`, sources the env file, runs the
playbook, and on success the playbook itself pushes a timestamp metric to
the monitoring stack's pushgateway. Grafana alerts (NoData → Alerting) if
the metric goes stale for more than 8 days.

`mgmt-host` itself is **not** in the `infra` tag pool, so the playbook
never updates the host it's running on. Update it manually a few times a
year with `apt update && apt dist-upgrade`.
