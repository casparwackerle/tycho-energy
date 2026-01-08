#!/usr/bin/env bash
set -euo pipefail

ROOT=/root/Documents/git/tycho-energy
PY="$ROOT/.venv/bin/python3"
SCRIPT="$ROOT/scripts/redfish_delay_measurement.py"

exec "$PY" "$SCRIPT" \
  --host 192.168.10.157 \
  --user admin \
  --password password \
  --cpu-pct 80 \
  --trials 50

# RUN with:
# sudo systemd-run --unit=redfish-delay --description="Redfish delay calibration" /root/Documents/git/tycho-energy/scripts/run_redfish_delay.sh

# control with
# sudo systemctl stop redfish-delay
# sudo systemctl reset-failed redfish-delay

