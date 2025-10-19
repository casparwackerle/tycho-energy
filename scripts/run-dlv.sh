#!/usr/bin/env bash
set -euo pipefail

# --- Defaults (override via env) ---
KEPLER_BIN="${KEPLER_BIN:-_output/bin/linux_amd64/kepler}"
CONFIG_DIR="${CONFIG_DIR:-$HOME/Documents/git/tycho-energy/kepler.config}"
REDFISH_CSV="${REDFISH_CSV:-$HOME/Documents/git/redfish.csv}"
PORT="${PORT:-40000}"
VERBOSE="${VERBOSE:-5}"
ENABLE_MSR="${ENABLE_MSR:-false}"
ENABLE_CGROUP_ID="${ENABLE_CGROUP_ID:-true}"
EXPOSE_HW="${EXPOSE_HW:-true}"   # --expose-hardware-counter-metrics

# --- Find dlv ---
if [[ -n "${DLV:-}" && -x "$DLV" ]]; then
  DLV_BIN="$DLV"
elif DLV_BIN="$(command -v dlv 2>/dev/null)"; then :; else
  echo "ERROR: dlv not found. Install: go install github.com/go-delve/delve/cmd/dlv@latest" >&2
  exit 1
fi

# --- Resolve and check paths ---
KEPLER_BIN="$(readlink -f "$KEPLER_BIN")"
CONFIG_DIR="$(readlink -f "$CONFIG_DIR")" || true
[[ -x "$KEPLER_BIN" ]] || { echo "ERROR: kepler binary not found/executable: $KEPLER_BIN" >&2; exit 1; }
[[ -d "$CONFIG_DIR" ]] || { echo "ERROR: config dir not found: $CONFIG_DIR" >&2; exit 1; }

REDFISH_FLAG=()
if [[ -f "$REDFISH_CSV" ]]; then
  REDFISH_CSV="$(readlink -f "$REDFISH_CSV")"
  REDFISH_FLAG=(--redfish-cred-file-path "$REDFISH_CSV")
fi

echo "Starting dlv (root) on 127.0.0.1:${PORT}"
echo "  dlv        : $DLV_BIN"
echo "  kepler     : $KEPLER_BIN"
echo "  config-dir : $CONFIG_DIR"
echo "  redfish    : ${REDFISH_FLAG:+$REDFISH_CSV}"
echo

# --- Build env pass-through for sudo ---
# 1) explicit whitelist for legacy/kepler flags
ENV_VARS_TO_PASS=(
  KEPLER_LOG_LEVEL ENABLE_EBPF_CGROUPID ENABLE_GPU ENABLE_PROCESS_METRICS ENABLE_QAT
  EXPOSE_CGROUP_METRICS EXPOSE_HW_COUNTER_METRICS EXPOSE_IRQ_COUNTER_METRICS EXPOSE_BPF_METRICS
  REDFISH_PROBE_INTERVAL_IN_SECONDS REDFISH_SKIP_SSL_VERIFY
  CGROUP_METRICS CPU_ARCH_OVERRIDE METRIC_PATH BIND_ADDRESS
  CONFIG_DIR KEPLER_BIN REDFISH_CSV VERBOSE ENABLE_MSR ENABLE_CGROUP_ID EXPOSE_HW
  NODE_NAME NODE_IP
)

sudo_env=()

# include whitelist if set
for v in "${ENV_VARS_TO_PASS[@]}"; do
  if [[ -n "${!v+x}" ]]; then
    sudo_env+=("$v=${!v}")
  fi
done

# 2) ALSO include every TYCHO_* that is set (this fixes your collector flags)
while IFS='=' read -r k v; do
  sudo_env+=("$k=$v")
done < <(env | grep -E '^TYCHO_' || true)

# --- Launch (root) with env preserved ---
sudo "${sudo_env[@]}" "$DLV_BIN" --listen=127.0.0.1:"$PORT" --headless --api-version=2 \
  --only-same-user=false \
  exec "$KEPLER_BIN" -- \
  --config-dir "$CONFIG_DIR" \
  "${REDFISH_FLAG[@]}" \
  --expose-hardware-counter-metrics="$EXPOSE_HW" \
  --enable-cgroup-id="$ENABLE_CGROUP_ID" \
  --enable-msr="$ENABLE_MSR" \
  --v="$VERBOSE"
