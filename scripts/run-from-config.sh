#!/usr/bin/env bash
# Read YAML (values-style) and run Kepler under Delve with matching settings.
# Expects mikefarah/yq v4. Your config keys live under: extraEnvVars: { KEY: VAL, ... }

# ---- do NOT allow sourcing (prevents poisoning your interactive shell) ----
if [[ "${BASH_SOURCE[0]}" != "${0}" ]]; then
  echo "Please RUN this script (do not source it): $0" >&2
  return 1
fi

set -Eeuo pipefail
trap 'echo "[run-from-config] error on line $LINENO"; exit 1' ERR

# ---- locate yq ----
if ! command -v yq >/dev/null 2>&1; then
  echo "ERROR: 'yq' not found. Install (e.g., 'sudo apt install yq') and retry." >&2
  exit 1
fi

# ---- inputs & defaults ----
CONFIG_YAML="${1:-"$HOME/Documents/git/tycho-energy/kepler.config/config.yaml"}"
[[ -f "$CONFIG_YAML" ]] || { echo "ERROR: config YAML not found: $CONFIG_YAML" >&2; exit 1; }

# Read helper: prefer extraEnvVars.KEY, fall back to top-level KEY. Never fail.
read_cfg() {
  local key="$1" v
  v="$(yq -r ".extraEnvVars.\"$key\" // \"\"" "$CONFIG_YAML" 2>/dev/null || true)"
  [[ -n "$v" && "$v" != "null" ]] && { printf "%s" "$v"; return; }
  v="$(yq -r ".\"$key\" // \"\"" "$CONFIG_YAML" 2>/dev/null || true)"
  [[ "$v" == "null" ]] && v=""
  printf "%s" "$v"
}

CONFIG_DIR_FROM_YAML="$(read_cfg CONFIG_DIR)"
CONFIG_DIR="${CONFIG_DIR_FROM_YAML:-"$(dirname "$(readlink -f "$CONFIG_YAML")")"}"

KEPLER_BIN="${KEPLER_BIN:-$(yq -r '.KEPLER_BIN // "_output/bin/linux_amd64/kepler"' "$CONFIG_YAML" 2>/dev/null || echo "_output/bin/linux_amd64/kepler")}"
REDFISH_CSV="$(yq -r '.REDFISH_CSV // ""' "$CONFIG_YAML" 2>/dev/null || echo "")"
[[ -z "$REDFISH_CSV" || "$REDFISH_CSV" == "null" ]] && REDFISH_CSV="$(yq -r '.redfish.credFilePath // ""' "$CONFIG_YAML" 2>/dev/null || echo "")"

# ---- robust export (no shell parsing of KEY=VAL) ----
safe_export() {
  local key="$1" raw val
  raw="$(read_cfg "$key" 2>/dev/null || true)"
  # strip CRs and ensure it's a single line
  val="$(printf '%s' "$raw" | tr -d '\r')"
  if [[ -n "$val" ]]; then
    # assign by name without eval; creates the variable if missing
    printf -v "$key" '%s' "$val"
    export "$key"
    echo "[cfg] $key=$val"
  fi
}

# ---- export commonly used envs (as seen in your DS) ----
for k in \
  KEPLER_LOG_LEVEL \
  ENABLE_EBPF_CGROUPID \
  ENABLE_GPU \
  ENABLE_PROCESS_METRICS \
  ENABLE_QAT \
  EXPOSE_CGROUP_METRICS \
  EXPOSE_HW_COUNTER_METRICS \
  EXPOSE_IRQ_COUNTER_METRICS \
  REDFISH_PROBE_INTERVAL_IN_SECONDS \
  REDFISH_SKIP_SSL_VERIFY \
  CGROUP_METRICS \
  CPU_ARCH_OVERRIDE \
  METRIC_PATH \
  BIND_ADDRESS
do
  safe_export "$k"
done
# ---- derive values used by run-dlv.sh (with safe defaults) ----
# VERBOSE from KEPLER_LOG_LEVEL
export VERBOSE="${KEPLER_LOG_LEVEL:-${VERBOSE:-5}}"

# ENABLE_CGROUP_ID from ENABLE_EBPF_CGROUPID (normalize)
case "${ENABLE_EBPF_CGROUPID:-true}" in
  1|true|"\"true\"" ) export ENABLE_CGROUP_ID=true ;;
  0|false|"\"false\"" ) export ENABLE_CGROUP_ID=false ;;
  * ) export ENABLE_CGROUP_ID="${ENABLE_EBPF_CGROUPID:-true}" ;;
esac

# EXPOSE_HW from EXPOSE_HW_COUNTER_METRICS
case "${EXPOSE_HW_COUNTER_METRICS:-true}" in
  1|true|"\"true\"" ) export EXPOSE_HW=true ;;
  0|false|"\"false\"" ) export EXPOSE_HW=false ;;
  * ) export EXPOSE_HW="${EXPOSE_HW_COUNTER_METRICS:-true}" ;;
esac

# ENABLE_MSR default (you had this block commented; keep it safe)
export ENABLE_MSR="${ENABLE_MSR:-false}"

# Provide paths to run-dlv.sh
export CONFIG_DIR
export KEPLER_BIN
[[ -n "$REDFISH_CSV" && "$REDFISH_CSV" != "null" ]] && export REDFISH_CSV

# ---- sanity prints (NEVER expand unset with -u) ----
echo "Config file   : $CONFIG_YAML"
echo "Config dir    : $CONFIG_DIR"
echo "Kepler binary : $KEPLER_BIN"
echo "Redfish CSV   : ${REDFISH_CSV:-<none>}"
echo "Flags/env ->   VERBOSE=${VERBOSE} ENABLE_CGROUP_ID=${ENABLE_CGROUP_ID} EXPOSE_HW=${EXPOSE_HW} ENABLE_MSR=${ENABLE_MSR}"
echo "Picked envs  -> ENABLE_GPU=${ENABLE_GPU-<unset>} ENABLE_PROCESS_METRICS=${ENABLE_PROCESS_METRICS-<unset>} CGROUP_METRICS=${CGROUP_METRICS-<unset>} BIND_ADDRESS=${BIND_ADDRESS-<unset>} METRIC_PATH=${METRIC_PATH-<unset>}"
echo

# ---- call the launcher (no exec; do not replace shell) ----
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$SCRIPT_DIR/run-dlv.sh"
