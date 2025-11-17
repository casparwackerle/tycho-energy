#!/usr/bin/env python3
"""
numba_gpu_stress_nvml.py

GPU stress + NVML power logging using:
- Numba CUDA for GPU load
- pynvml for power readings

For each run:
- Idle phase: sample NVML power for a given duration.
- Burn phase: repeatedly launch a Numba kernel while sampling NVML.

Idle stats:
- The idle phase runs for --idle-seconds, but only the *last half* of the
  idle samples is used to compute idle mean and stddev. This helps to ignore
  initial transients and focus on the stationary idle plateau.

Delay detection:
- For each GPU, compute idle mean and stddev (in mW) from the last-half idle samples.
- Define a dynamic threshold:
      threshold = idle_mean + max(delta_watts, sigma_multiplier * idle_stddev)
- During burn, detect the first time power crosses this threshold.
- The delay is the corresponding time (ms) since burn start.

After all runs:
- Aggregate all valid delays across runs and GPUs.
- Compute min, median, mean, p5, max.
- Recommend p5 as the "fastest reliable" reaction time.
"""

import argparse
import sys
import time
import math

import numpy as np
from numba import cuda

# Dependency check: pynvml from python3-pynvml (or nvidia-ml-py3 via pip).
try:
    import pynvml
except ImportError:
    print("ERROR: The 'pynvml' module is not available.")
    print("       Please install it, for example with:")
    print("         sudo apt install python3-pynvml")
    print("       (Alternatively: pip install nvidia-ml-py3)")
    sys.exit(1)


# -------------------------------------------------------------------
# GPU info + NVML helpers
# -------------------------------------------------------------------

class GpuInfo:
    def __init__(self, index, handle, name):
        self.index = index
        self.handle = handle
        self.name = name


def parse_args():
    parser = argparse.ArgumentParser(
        description="Stress an NVIDIA GPU with Numba and log NVML power."
    )
    parser.add_argument(
        "--runs",
        type=int,
        default=5,
        help="Number of repeated idle+burn runs (default: %(default)s)",
    )
    parser.add_argument(
        "--idle-seconds",
        type=float,
        default=10.0,
        help="Idle measurement duration before stress (default: %(default)s s)",
    )
    parser.add_argument(
        "--burn-seconds",
        type=float,
        default=10.0,
        help="Duration (in seconds) to stress the GPU (default: %(default)s)",
    )
    parser.add_argument(
        "--sample-interval-ms",
        type=float,
        default=50.0,
        help="Sampling interval in milliseconds (default: %(default)s ms)",
    )
    parser.add_argument(
        "--elements",
        type=int,
        default=10_000_000,
        help="Number of FP32 elements processed by the kernel (default: %(default)s)",
    )
    parser.add_argument(
        "--iters",
        type=int,
        default=2000,
        help="Number of arithmetic iterations per element (default: %(default)s)",
    )
    parser.add_argument(
        "--gpu-index",
        type=int,
        default=0,
        help="GPU index to run on (default: %(default)s)",
    )
    parser.add_argument(
        "--delta-watts",
        type=float,
        default=1.0,
        help=(
            "Minimum absolute increase over idle mean in watts to "
            "consider a reaction (default: %(default)s W)"
        ),
    )
    parser.add_argument(
        "--sigma-multiplier",
        type=float,
        default=2.0,
        help=(
            "Multiplier for idle stddev in threshold computation.\n"
            "Threshold is idle_mean + max(delta_watts, sigma_multiplier * idle_stddev). "
            "(default: %(default)s)"
        ),
    )
    parser.add_argument(
        "--verbose",
        action="store_true",
        help="Print additional details.",
    )
    return parser.parse_args()


def init_nvml_and_gpus():
    """Initialize NVML and return list of GpuInfo objects."""
    try:
        pynvml.nvmlInit()
    except pynvml.NVMLError as e:
        print(f"ERROR: Failed to initialize NVML: {e}")
        sys.exit(1)

    device_count = pynvml.nvmlDeviceGetCount()
    if device_count == 0:
        print("ERROR: No NVML-compatible GPUs found.")
        sys.exit(1)

    gpus = []
    for idx in range(device_count):
        handle = pynvml.nvmlDeviceGetHandleByIndex(idx)

        raw_name = pynvml.nvmlDeviceGetName(handle)
        if isinstance(raw_name, bytes):
            name = raw_name.decode("utf-8", errors="ignore")
        else:
            name = str(raw_name)

        # Check power support
        try:
            _ = pynvml.nvmlDeviceGetPowerUsage(handle)
        except pynvml.NVMLError as e:
            if hasattr(pynvml, "NVML_ERROR_NOT_SUPPORTED") and getattr(e, "value", None) == pynvml.NVML_ERROR_NOT_SUPPORTED:
                print(f"WARNING: GPU {idx} ({name}) does not support power readings, skipping.")
                continue
            else:
                print(f"ERROR: Failed to read power for GPU {idx} ({name}): {e}")
                sys.exit(1)

        gpus.append(GpuInfo(idx, handle, name))

    if not gpus:
        print("ERROR: No GPUs with NVML power readings available.")
        sys.exit(1)

    return gpus


def read_powers_mw(gpus):
    """Return (timestamp_seconds, [power_mw_per_gpu])."""
    t = time.time()
    powers = []
    for gpu in gpus:
        p = pynvml.nvmlDeviceGetPowerUsage(gpu.handle)  # mW
        powers.append(p)
    return t, powers


def mean(values):
    return sum(values) / len(values) if values else 0.0


def stddev(values, mu=None):
    n = len(values)
    if n < 2:
        return 0.0
    if mu is None:
        mu = mean(values)
    var = sum((x - mu) ** 2 for x in values) / n
    return math.sqrt(var)


def percentile(sorted_values, p):
    """
    Nearest-rank-like percentile on a sorted list (ascending).
    p in [0, 100].
    """
    if not sorted_values:
        return None
    if p <= 0:
        return sorted_values[0]
    if p >= 100:
        return sorted_values[-1]
    k = (p / 100.0) * (len(sorted_values) - 1)
    idx = int(round(k))
    return sorted_values[idx]


# -------------------------------------------------------------------
# Numba kernel + stress helpers
# -------------------------------------------------------------------

@cuda.jit
def burn_kernel(a, iters):
    """
    Simple GPU workload:
    For each element in array a, perform `iters` FP32 operations.

    a: device array
    iters: number of arithmetic iterations
    """
    i = cuda.grid(1)
    if i >= a.size:
        return

    x = a[i]
    for k in range(iters):
        x = x * 1.0000001 + 1.0
    a[i] = x


def prepare_device_array(n_elements, gpu_index=0, verbose=False):
    """
    Select device, allocate device array, and warm up the kernel
    to ensure CUDA context + JIT compilation overhead is paid upfront.
    """
    cuda.select_device(gpu_index)

    if verbose:
        print(f"[info] Selected CUDA device {gpu_index}")

    h_a = np.ones(n_elements, dtype=np.float32)
    d_a = cuda.to_device(h_a)

    threads = 256
    blocks = (n_elements + threads - 1) // threads

    if verbose:
        print("[info] Warming up CUDA context and JIT compilation...")
    burn_kernel[blocks, threads](d_a, 1)
    cuda.synchronize()
    if verbose:
        print("[info] Warm-up done.")

    return d_a, blocks, threads


# -------------------------------------------------------------------
# Single run: idle + burn + delay detection
# -------------------------------------------------------------------

def run_single_measurement(run_index, args, gpus, d_a, blocks, threads):
    """
    Perform one idle+burn run:

    - Idle phase: NVML power sampling only.
    - Burn phase: repeated kernel launches + NVML sampling.

    Idle stats:
      Use only the *last half* of idle samples for mean/stddev.

    Delay detection:
      For each GPU, we compute idle mean and stddev (in mW) from the last half
      of idle samples and define:

         threshold = idle_mean
                     + max(delta_watts, sigma_multiplier * idle_stddev)

      Then we detect the first time during the burn phase when
         power >= threshold.

    Returns:
        delays_ms: list of per-GPU delays (ms) or None if not detected
    """

    sample_interval = args.sample_interval_ms / 1000.0
    num_gpus = len(gpus)

    idle_samples = [[] for _ in range(num_gpus)]

    print(f"\n=== Run {run_index} / {args.runs} ===")
    print(f"Using {len(gpus)} GPU(s) with NVML power support:")
    for gpu in gpus:
        print(f"  GPU {gpu.index}: {gpu.name}")

    # ----------------------------------------------------------------
    # Idle phase
    # ----------------------------------------------------------------
    print(f"\nMeasuring idle power for {args.idle_seconds:.1f} s...")
    idle_start = time.time()
    last_sample = idle_start

    while (time.time() - idle_start) < args.idle_seconds:
        t_now, powers = read_powers_mw(gpus)
        t_rel_ms = (t_now - idle_start) * 1000.0

        for i, p in enumerate(powers):
            idle_samples[i].append(p)

        line = [f"[idle] t={t_rel_ms:8.1f} ms"]
        for i, gpu in enumerate(gpus):
            line.append(f"GPU{gpu.index}: {powers[i] / 1000:.1f} W")
        print("  " + "  ".join(line), flush=True)

        elapsed = time.time() - last_sample
        to_sleep = sample_interval - elapsed
        if to_sleep > 0:
            time.sleep(to_sleep)
        last_sample = time.time()

    # Use only the last half of the idle samples for stats
    idle_mean_mw = []
    idle_std_mw = []

    for i in range(num_gpus):
        samples = idle_samples[i]
        if not samples:
            idle_mean_mw.append(0.0)
            idle_std_mw.append(0.0)
            continue

        half_idx = len(samples) // 2
        trimmed = samples[half_idx:]  # last half
        mu = mean(trimmed)
        sigma = stddev(trimmed, mu)

        idle_mean_mw.append(mu)
        idle_std_mw.append(sigma)

    if args.verbose:
        print("\nIdle statistics (last half of idle window):")
        for i, gpu in enumerate(gpus):
            print(
                f"  GPU {gpu.index} idle mean: {idle_mean_mw[i] / 1000:.2f} W, "
                f"stddev: {idle_std_mw[i] / 1000:.3f} W "
                f"(from {len(idle_samples[i]) // 2} samples)"
            )

    # Dynamic thresholds per GPU
    delta_mw = args.delta_watts * 1000.0
    thresholds_mw = []
    for mu, sigma in zip(idle_mean_mw, idle_std_mw):
        extra = max(delta_mw, args.sigma_multiplier * sigma)
        thresholds_mw.append(mu + extra)

    delays_ms = [None] * num_gpus

    # ----------------------------------------------------------------
    # Burn phase
    # ----------------------------------------------------------------
    print(f"\nStarting GPU stress for {args.burn_seconds:.1f} s...")
    burn_start_time = time.time()
    burn_start_ns = time.perf_counter_ns()
    print(f"[info] burn_start_ns = {burn_start_ns}")

    last_sample = burn_start_time

    while (time.time() - burn_start_time) < args.burn_seconds:
        burn_kernel[blocks, threads](d_a, args.iters)
        cuda.synchronize()

        t_now, powers = read_powers_mw(gpus)
        t_rel_ms = (t_now - burn_start_time) * 1000.0

        line = [f"[run ] t={t_rel_ms:8.1f} ms"]
        for i, gpu in enumerate(gpus):
            p_mw = powers[i]
            line.append(f"GPU{gpu.index}: {p_mw / 1000:.1f} W")

            # First time power crosses threshold => delay
            if delays_ms[i] is None and p_mw >= thresholds_mw[i]:
                delays_ms[i] = t_rel_ms

        print("  " + "  ".join(line), flush=True)

        elapsed = time.time() - last_sample
        to_sleep = sample_interval - elapsed
        if to_sleep > 0:
            time.sleep(to_sleep)
        last_sample = time.time()

    burn_end_ns = time.perf_counter_ns()
    print(f"[info] burn_end_ns   = {burn_end_ns}")
    print(f"[info] burn duration ≈ {(burn_end_ns - burn_start_ns)/1e9:.6f} s\n")

    print("Run delay summary (dynamic threshold over last-half idle stats):")
    for i, gpu in enumerate(gpus):
        idle_w = idle_mean_mw[i] / 1000.0
        sigma_w = idle_std_mw[i] / 1000.0
        threshold_w = thresholds_mw[i] / 1000.0
        if delays_ms[i] is not None:
            print(
                f"  GPU {gpu.index}: idle={idle_w:.2f} W, "
                f"σ={sigma_w:.3f} W, threshold={threshold_w:.2f} W "
                f"-> delay ≈ {delays_ms[i]:.1f} ms"
            )
        else:
            print(
                f"  GPU {gpu.index}: idle={idle_w:.2f} W, "
                f"σ={sigma_w:.3f} W, threshold={threshold_w:.2f} W "
                f"-> delay not detected."
            )

    return delays_ms


# -------------------------------------------------------------------
# Main
# -------------------------------------------------------------------

def main():
    args = parse_args()
    gpus = init_nvml_and_gpus()

    # Prepare CUDA array + warm-up ONCE, reused across runs
    d_a, blocks, threads = prepare_device_array(
        args.elements,
        gpu_index=args.gpu_index,
        verbose=args.verbose,
    )

    all_delays = []  # flattened list of all valid delays (ms)

    try:
        for run_index in range(1, args.runs + 1):
            delays_ms = run_single_measurement(run_index, args, gpus, d_a, blocks, threads)
            for d in delays_ms:
                if d is not None:
                    all_delays.append(d)
    finally:
        try:
            pynvml.nvmlShutdown()
        except Exception:
            pass

    if not all_delays:
        print("\nERROR: No valid delay measurements were obtained.")
        sys.exit(1)

    all_delays_sorted = sorted(all_delays)
    d_min = all_delays_sorted[0]
    d_max = all_delays_sorted[-1]
    d_med = percentile(all_delays_sorted, 50)
    d_mean = mean(all_delays_sorted)
    d_p5 = percentile(all_delays_sorted, 5)

    print("\n=== Aggregated delay statistics (all runs, all GPUs) ===")
    print(f"Total valid measurements: {len(all_delays)}")
    print(f"  min   : {d_min:.1f} ms")
    print(f"  median: {d_med:.1f} ms")
    print(f"  mean  : {d_mean:.1f} ms")
    print(f"  p5    : {d_p5:.1f} ms")
    print(f"  max   : {d_max:.1f} ms")

    print("\nRecommended (5th percentile) GPU delay:")
    print(f"  {d_p5:.1f} ms")

    sys.exit(0)


if __name__ == "__main__":
    main()
