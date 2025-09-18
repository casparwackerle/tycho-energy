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

# --- Launch (root) ---
sudo "$DLV_BIN" --listen=127.0.0.1:"$PORT" --headless --api-version=2 \
  --only-same-user=false \
  exec "$KEPLER_BIN" -- \
  --config-dir "$CONFIG_DIR" \
  "${REDFISH_FLAG[@]}" \
  --expose-hardware-counter-metrics="$EXPOSE_HW" \
  --enable-cgroup-id="$ENABLE_CGROUP_ID" \
  --enable-msr="$ENABLE_MSR" \
  --v="$VERBOSE"

