#!/bin/sh
# Wrapper for the ansible infra update playbook.
#
# Sources secrets from /etc/homelab-manager/ansible.env, runs the playbook,
# logs to /var/log/ansible-infra-update.log, and pushes observability
# metrics to the monitoring pushgateway on success AND failure.
#
# Metrics pushed (to job ansible_infra_update):
#   ansible_infra_update_last_run_timestamp         — unix ts, every run
#   ansible_infra_update_last_run_duration_seconds  — wall-clock duration
#   ansible_infra_update_last_run_status            — 1 = success, 0 = fail
#   ansible_infra_update_last_success_timestamp     — success runs only
#
# Pushgateway receives POST, so metrics missing from a given push are
# left untouched. That's how last_success_timestamp stays "sticky"
# across failed runs — the dead-man's switch only trips if NO run
# succeeds for >8 days (see Grafana alert rule ansible-infra-updates-stale).
#
# Run by /etc/cron.d/ansible-infra-update — also safe to invoke manually.

set -u

ENV_FILE=/etc/homelab-manager/ansible.env
PLAYBOOK=/opt/homelab-manager/ansible/playbooks/update.yml
LOG=/var/log/ansible-infra-update.log
PUSHGATEWAY=http://10.0.0.6:9091
JOB=ansible_infra_update

if [ ! -r "$ENV_FILE" ]; then
  echo "FATAL: cannot read $ENV_FILE" >&2
  exit 1
fi
. "$ENV_FILE"
export PROXMOX_TOKEN_SECRET

cd /opt/homelab-manager/ansible
export LANG=en_US.UTF-8 LC_ALL=en_US.UTF-8

start_ts=$(date +%s)
rc=1
end_ts=$start_ts
duration=0

{
  echo
  echo "=== ansible-infra-update run started $(date -Is) ==="
  ansible-playbook "$PLAYBOOK"
  rc=$?
  end_ts=$(date +%s)
  duration=$((end_ts - start_ts))
  echo "=== run finished $(date -Is) (rc=$rc, duration=${duration}s) ==="
} >> "$LOG" 2>&1

status=0
[ "$rc" -eq 0 ] && status=1

{
  printf '# TYPE ansible_infra_update_last_run_timestamp gauge\n'
  printf 'ansible_infra_update_last_run_timestamp %s\n' "$end_ts"
  printf '# TYPE ansible_infra_update_last_run_duration_seconds gauge\n'
  printf 'ansible_infra_update_last_run_duration_seconds %s\n' "$duration"
  printf '# TYPE ansible_infra_update_last_run_status gauge\n'
  printf 'ansible_infra_update_last_run_status %s\n' "$status"
  if [ "$status" -eq 1 ]; then
    printf '# TYPE ansible_infra_update_last_success_timestamp gauge\n'
    printf 'ansible_infra_update_last_success_timestamp %s\n' "$end_ts"
  fi
} | curl -sf --max-time 10 --data-binary @- "$PUSHGATEWAY/metrics/job/$JOB" \
    >> "$LOG" 2>&1 \
  || echo "WARNING: failed to push metrics to $PUSHGATEWAY" >> "$LOG"

exit "$rc"
