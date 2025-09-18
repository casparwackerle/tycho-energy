#!/usr/bin/env bash
# Run Kepler under Delve (headless) with the paths you’ve been using.
# Override defaults via env vars (see below).

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

# --- Find dlv (use $DLV if already exported in your shell) ---
if [[ -n "${DLV:-}" && -x "$DLV" ]]; then
  DLV_BIN="$DLV"
elif DLV_BIN="$(command -v dlv 2>/dev/null)"; then
  :
else
  echo "ERROR: dlv not found. Install with: go install github.com/go-delve/delve/cmd/dlv@latest" >&2
  exit 1
fi

# --- Resolve and check paths ---
KEPLER_BIN="$(readlink -f "$KEPLER_BIN")"
CONFIG_DIR="$(readlink -f "$CONFIG_DIR")" || true

if [[ ! -x "$KEPLER_BIN" ]]; then
  echo "ERROR: kepler binary not found or not executable: $KEPLER_BIN" >&2
  exit 1
fi
if [[ ! -d "$CONFIG_DIR" ]]; then
  echo "ERROR: config dir not found: $CONFIG_DIR" >&2
  exit 1
fi

REDFISH_FLAG=()
if [[ -f "$REDFISH_CSV" ]]; then
  REDFISH_CSV="$(readlink -f "$REDFISH_CSV")"
  REDFISH_FLAG=(--redfish-cred-file-path "$REDFISH_CSV")
fi

# build args list safely
args=( --config-dir "$CONFIG_DIR" )
[[ -f "$REDFISH_CSV" ]] && args+=( --redfish-cred-file-path "$REDFISH_CSV" )
[[ -n "${ENABLE_GPU:-}" ]]               && args+=( --enable-gpu="${ENABLE_GPU}" )
[[ -n "${ENABLE_PROCESS_METRICS:-}" ]]   && args+=( --enable-process-metrics="${ENABLE_PROCESS_METRICS}" )
[[ -n "${EXPOSE_IRQ_COUNTER_METRICS:-}" ]] && args+=( --expose-irq-counter-metrics="${EXPOSE_IRQ_COUNTER_METRICS}" )
[[ -n "${EXPOSE_CGROUP_METRICS:-}" ]]    && args+=( --expose-cgroup-metrics="${EXPOSE_CGROUP_METRICS}" )
args+=( --expose-hardware-counter-metrics="$EXPOSE_HW" --enable-cgroup-id="$ENABLE_CGROUP_ID" --enable-msr="$ENABLE_MSR" --v="$VERBOSE" )
# then use: exec "$KEPLER_BIN" -- "${args[@]}"

echo "Starting dlv (root) on 127.0.0.1:${PORT}"
echo "  dlv        : $DLV_BIN"
echo "  kepler     : $KEPLER_BIN"
echo "  config-dir : $CONFIG_DIR"
if [[ ${#REDFISH_FLAG[@]} -gt 0 ]]; then
  echo "  redfish    : $REDFISH_CSV"
else
  echo "  redfish    : (none)"
fi
echo

# --- Build env pass-through for sudo (only vars that are set) ---
ENV_VARS_TO_PASS=(
  KEPLER_LOG_LEVEL ENABLE_EBPF_CGROUPID ENABLE_GPU ENABLE_PROCESS_METRICS ENABLE_QAT
  EXPOSE_CGROUP_METRICS EXPOSE_HW_COUNTER_METRICS EXPOSE_IRQ_COUNTER_METRICS EXPOSE_BPF_METRICS
  REDFISH_PROBE_INTERVAL_IN_SECONDS REDFISH_SKIP_SSL_VERIFY
  CGROUP_METRICS CPU_ARCH_OVERRIDE METRIC_PATH BIND_ADDRESS
  NODE_NAME NODE_IP
)
sudo_env=()
for v in "${ENV_VARS_TO_PASS[@]}"; do
  if [[ -n "${!v+x}" ]]; then
    sudo_env+=("$v=${!v}")
  fi
done

# --- Launch (root) with env preserved ---
# sudo "${sudo_env[@]}" "$DLV_BIN" --listen=127.0.0.1:"$PORT" --headless --api-version=2 \
#   --only-same-user=false \
#   exec "$KEPLER_BIN" -- \
#   --config-dir "$CONFIG_DIR" \
#   "${REDFISH_FLAG[@]}" \
#   --expose-hardware-counter-metrics="$EXPOSE_HW" \
#   --enable-cgroup-id="$ENABLE_CGROUP_ID" \
#   --enable-msr="$ENABLE_MSR" \
#   --v="$VERBOSE"

sudo "${sudo_env[@]}" "$DLV_BIN" --listen=127.0.0.1:"$PORT" --headless --api-version=2 \
  --only-same-user=false \
  exec "$KEPLER_BIN" -- \
  --config-dir "$CONFIG_DIR" \
  "${REDFISH_FLAG[@]}" \
  "${GPU_FLAG[@]}" \
  "${PROC_FLAG[@]}" \
  "${IRQ_FLAG[@]}" \
  "${CGROUP_METRICS_FLAG[@]}" \
  --expose-hardware-counter-metrics="$EXPOSE_HW" \
  --enable-cgroup-id="$ENABLE_CGROUP_ID" \
  --enable-msr="$ENABLE_MSR" \
  --v="$VERBOSE"