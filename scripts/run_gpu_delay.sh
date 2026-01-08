#!/usr/bin/env bash
set -euo pipefail

ROOT=/root/Documents/git/tycho-energy
PY="$ROOT/.venv/bin/python3"
SCRIPT="$ROOT/scripts/gpu_delay_measurement.py"

exec "$PY" "$SCRIPT" \
  --runs 500 \
  --idle-seconds 10 \
  --burn-seconds 10 \
  --sample-interval-ms 50 \
  --elements 30000000 \
  --iters 5000 \
  --gpu-indices 0,1 \
  --delta-watts 2.0 \
  --sigma-multiplier 3 \
  --rel-increase 0.05 \
  --cooldown-seconds 30 \
  --hist \
  --hist-bins 50


# RUN with:
# sudo systemd-run --unit=gpu-delay --description="GPU delay calibration" /root/Documents/git/tycho-energy/scripts/run_gpu_delay.sh

# control with
# sudo systemctl stop gpu-delay
# sudo systemctl reset-failed gpu-delay