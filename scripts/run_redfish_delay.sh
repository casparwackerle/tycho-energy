#!/usr/bin/env bash
set -euo pipefail

ROOT=/root/Documents/git/tycho-energy
PY="$ROOT/.venv/bin/python3"
SCRIPT="$ROOT/scripts/redfish_delay_measurement.py"

ARGS=(
  --host "192.168.10.157"
  --user "admin"
  --password "password"
  --cpu-pct "80"
  --trials "20"
  --insecure
)

echo "[debug wrapper] PY=$PY"
echo "[debug wrapper] SCRIPT=$SCRIPT"
printf '[debug wrapper] ARGS:'; printf ' %q' "${ARGS[@]}"; echo

exec "$PY" "$SCRIPT" "${ARGS[@]}"

# RUN with:
# sudo systemd-run --unit=redfish-delay --description="Redfish delay calibration" --collect --property=TimeoutStopUSec=30s /root/Documents/git/tycho-energy/scripts/run_redfish_delay.sh

# control with
# sudo systemctl stop redfish-delay
# sudo systemctl reset-failed redfish-delay

# live logs:
# sudo journalctl -u redfish-delay -f

# logs after completion:
# sudo journalctl -u redfish-delay
# sudo journalctl -u redfish-delay --no-pager -o short-precise

