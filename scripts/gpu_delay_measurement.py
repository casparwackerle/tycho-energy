#!/usr/bin/env python3
"""
numba_gpu_stress_nvml.py

Multi-GPU stress (Numba CUDA) + NVML power logging + delay detection.

Fixes vs older version:
- Robust threshold: idle_mean + max(delta_watts, sigma_multiplier*sigma, rel_increase*idle_mean)
- Debounce: require N consecutive samples above threshold to avoid near-zero false triggers
- Per-GPU delay statistics (and global)
- Monotonic timing for delays (time.perf_counter)
- Multiprocessing spawn (CUDA-safe) + non-daemon workers
"""

import argparse
import sys
import time
import math
import signal
import multiprocessing as mp
import os
from datetime import datetime

import matplotlib.pyplot as plt

import numpy as np
from numba import cuda

try:
    import pynvml
except ImportError:
    print("ERROR: The 'pynvml' module is not available.")
    print("       Please install it, for example with:")
    print("         sudo apt install python3-pynvml")
    print("       (Alternatively: pip install nvidia-ml-py3)")
    sys.exit(1)


class GpuInfo:
    def __init__(self, index, handle, name):
        self.index = index
        self.handle = handle
        self.name = name

 
def parse_gpu_indices(s: str):
    out = []
    for part in s.split(","):
        part = part.strip()
        if part:
            out.append(int(part))
    # stable unique
    seen = set()
    uniq = []
    for x in out:
        if x not in seen:
            seen.add(x)
            uniq.append(x)
    return uniq


def parse_args():
    p = argparse.ArgumentParser(description="Stress NVIDIA GPU(s) with Numba and log NVML power.")

    p.add_argument("--runs", type=int, default=50)
    p.add_argument("--idle-seconds", type=float, default=5.0)
    p.add_argument("--burn-seconds", type=float, default=5.0)
    p.add_argument("--sample-interval-ms", type=float, default=50.0)

    p.add_argument("--elements", type=int, default=30_000_000)
    p.add_argument("--iters", type=int, default=5000)

    p.add_argument("--gpu-indices", type=str, default="0,1", help='Comma-separated GPU indices to stress, e.g. "0,1"')

    p.add_argument("--hist", action="store_true", default="true", help="Write per-GPU delay histograms as PNG files")
    p.add_argument("--hist-bins", type=int, default=50, help="Histogram bins (default: %(default)s)")
    p.add_argument("--hist-outdir", type=str, default=None, help="Histogram output directory (default: %(default)s)")


    # Threshold controls
    p.add_argument("--delta-watts", type=float, default=2.0,
                   help="Minimum absolute increase over idle mean to count as reaction (default: %(default)s W)")
    p.add_argument("--sigma-multiplier", type=float, default=3,
                   help="Multiplier for idle stddev component in threshold (default: %(default)s)")
    p.add_argument("--rel-increase", type=float, default=0.05,
                   help="Minimum relative increase over idle mean (fraction, default: %(default)s)")
    p.add_argument("--cooldown-seconds", type=float, default=5.0, help="Cooldown time before each idle phase (default: %(default)s s)",
)

    # Debounce controls
    p.add_argument("--confirm-samples", type=int, default=2,
                   help="Require this many consecutive samples >= threshold (default: %(default)s)")

    # Optional: also sample NVML utilization (for sanity)
    p.add_argument("--sample-util", action="store_true",
                   help="Also sample NVML utilization (sanity check)")

    p.add_argument("--verbose", action="store_true")
    return p.parse_args()


def init_nvml_and_gpus():
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

        # power support check
        try:
            _ = pynvml.nvmlDeviceGetPowerUsage(handle)
        except pynvml.NVMLError as e:
            if hasattr(pynvml, "NVML_ERROR_NOT_SUPPORTED") and getattr(e, "value", None) == pynvml.NVML_ERROR_NOT_SUPPORTED:
                print(f"WARNING: GPU {idx} ({name}) does not support power readings, skipping.")
                continue
            raise

        gpus.append(GpuInfo(idx, handle, name))

    if not gpus:
        print("ERROR: No GPUs with NVML power readings available.")
        sys.exit(1)

    return gpus


def read_powers_mw(gpus):
    t = time.time()
    powers = [pynvml.nvmlDeviceGetPowerUsage(g.handle) for g in gpus]
    return t, powers


def read_utils(gpus):
    # returns list of (gpu_util%, mem_util%)
    out = []
    for g in gpus:
        u = pynvml.nvmlDeviceGetUtilizationRates(g.handle)
        out.append((int(u.gpu), int(u.memory)))
    return out


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
    if not sorted_values:
        return None
    if p <= 0:
        return sorted_values[0]
    if p >= 100:
        return sorted_values[-1]
    k = (p / 100.0) * (len(sorted_values) - 1)
    idx = int(round(k))
    return sorted_values[idx]


@cuda.jit
def burn_kernel(a, iters):
    i = cuda.grid(1)
    if i >= a.size:
        return
    x = a[i]
    for _ in range(iters):
        x = x * 1.0000001 + 1.0
    a[i] = x


def stress_worker(gpu_index, args, start_evt, stop_evt, ready_evt, verbose=False):
    """
    Own CUDA context per process. Burn when start_evt is set.
    """
    did_print_first = False
    try:
        cuda.select_device(gpu_index)

        h_a = np.ones(args.elements, dtype=np.float32)
        d_a = cuda.to_device(h_a)

        threads = 1024
        blocks = (args.elements + threads - 1) // threads

        # warm-up
        burn_kernel[blocks, threads](d_a, 1)
        cuda.synchronize()
        ready_evt.set()

        if verbose:
            print(f"[worker GPU{gpu_index}] ready", flush=True)

        while not stop_evt.is_set():
            if not start_evt.wait(timeout=0.05):
                continue

            while start_evt.is_set() and not stop_evt.is_set():
                burn_kernel[blocks, threads](d_a, args.iters)
                cuda.synchronize()
                if verbose and not did_print_first:
                    print(f"[worker GPU{gpu_index}] first kernel executed (this run)", flush=True)
                    did_print_first = True

            # reset per-run debug print
            did_print_first = False

    except Exception as e:
        print(f"[worker GPU{gpu_index}] ERROR: {e}", flush=True)
    finally:
        try:
            cuda.close()
        except Exception:
            pass


def compute_thresholds(idle_samples_per_gpu, args):
    idle_mean_mw = []
    idle_std_mw = []

    for samples in idle_samples_per_gpu:
        if not samples:
            idle_mean_mw.append(0.0)
            idle_std_mw.append(0.0)
            continue

        half_idx = len(samples) // 2
        trimmed = samples[half_idx:]
        mu = mean(trimmed)
        sigma = stddev(trimmed, mu)

        idle_mean_mw.append(mu)
        idle_std_mw.append(sigma)

    delta_mw = args.delta_watts * 1000.0
    thresholds_mw = []
    for mu, sigma in zip(idle_mean_mw, idle_std_mw):
        rel_mw = args.rel_increase * mu
        extra = max(delta_mw, args.sigma_multiplier * sigma, rel_mw)
        thresholds_mw.append(mu + extra)

    return idle_mean_mw, idle_std_mw, thresholds_mw


def main():
    # CUDA-safe multiprocessing
    mp.set_start_method("spawn", force=True)

    args = parse_args()
    if args.hist_outdir is None:
        script_dir = os.path.dirname(os.path.abspath(__file__))
    args.hist_outdir = script_dir
    gpus = init_nvml_and_gpus()

    gpu_indices_to_stress = parse_gpu_indices(args.gpu_indices)
    if not gpu_indices_to_stress:
        print("ERROR: --gpu-indices resulted in empty set")
        sys.exit(1)

    nvml_indices = {g.index for g in gpus}
    for gi in gpu_indices_to_stress:
        if gi not in nvml_indices:
            print(f"ERROR: Requested GPU index {gi}, but NVML power GPU list is {sorted(nvml_indices)}")
            sys.exit(1)

    print("Stressing GPU indices:", ", ".join(str(x) for x in gpu_indices_to_stress))
    sample_interval = args.sample_interval_ms / 1000.0

    start_evt = mp.Event()
    stop_evt = mp.Event()

    procs = []
    ready_evts = {}
    for gi in gpu_indices_to_stress:
        ready = mp.Event()
        ready_evts[gi] = ready
        p = mp.Process(target=stress_worker, args=(gi, args, start_evt, stop_evt, ready, args.verbose))
        p.start()
        procs.append((gi, p))

    # wait for readiness
    for gi, _p in procs:
        if not ready_evts[gi].wait(timeout=60.0):
            print(f"ERROR: Worker for GPU {gi} did not become ready in time.")
            stop_evt.set()
            for _, pp in procs:
                pp.join(timeout=2.0)
            sys.exit(1)

    def _stop_everything():
        start_evt.clear()
        stop_evt.set()

    def _sig_handler(_signum, _frame):
        print("\n[info] Caught interrupt, stopping workers...", flush=True)
        _stop_everything()

    signal.signal(signal.SIGINT, _sig_handler)
    signal.signal(signal.SIGTERM, _sig_handler)

    # per-GPU delay lists (by NVML GPU index)
    delays_by_gpu = {g.index: [] for g in gpus}

    try:
        for run_index in range(1, args.runs + 1):
            print(f"\n=== Run {run_index} / {args.runs} ===")
            print(f"Using {len(gpus)} GPU(s) with NVML power support:")
            for g in gpus:
                print(f"  GPU {g.index}: {g.name}")

            # ----------------------------
            # Idle phase (no stress)
            # ----------------------------
            start_evt.clear()
            idle_samples = [[] for _ in range(len(gpus))]
            
            start_evt.clear()  # ensure stress is off
            if args.cooldown_seconds and args.cooldown_seconds > 0:
                print(f"\nCooldown for {args.cooldown_seconds:.1f} s before idle measurement...")
                time.sleep(args.cooldown_seconds)

            print(f"\nMeasuring idle power for {args.idle_seconds:.1f} s...")
            idle_start = time.time()
            last_sample = idle_start

            while (time.time() - idle_start) < args.idle_seconds:
                t_now, powers = read_powers_mw(gpus)
                t_rel_ms = (t_now - idle_start) * 1000.0

                for i, p_mw in enumerate(powers):
                    idle_samples[i].append(p_mw)

                line = [f"[idle] t={t_rel_ms:8.1f} ms"]
                for i, gpu in enumerate(gpus):
                    line.append(f"GPU{gpu.index}: {powers[i] / 1000:.1f} W")
                print("  " + "  ".join(line), flush=True)

                elapsed = time.time() - last_sample
                to_sleep = sample_interval - elapsed
                if to_sleep > 0:
                    time.sleep(to_sleep)
                last_sample = time.time()

            idle_mean_mw, idle_std_mw, thresholds_mw = compute_thresholds(idle_samples, args)

            if args.verbose:
                print("\nThreshold details:")
                for i, gpu in enumerate(gpus):
                    mu = idle_mean_mw[i] / 1000.0
                    sig = idle_std_mw[i] / 1000.0
                    thr = thresholds_mw[i] / 1000.0
                    print(f"  GPU {gpu.index}: idle={mu:.2f} W, σ={sig:.3f} W, threshold={thr:.2f} W")

            # ----------------------------
            # Burn phase (stress + sample)
            # ----------------------------
            print(f"\nStarting GPU stress for {args.burn_seconds:.1f} s...")
            burn_start_wall = time.time()
            burn_t0 = time.perf_counter()  # monotonic for delays
            print(f"[info] burn_start_ns = {time.perf_counter_ns()}")

            start_evt.set()

            # Debounce counters per GPU
            above_cnt = [0] * len(gpus)
            detected_at_ms = [None] * len(gpus)

            last_sample = time.time()
            while (time.time() - burn_start_wall) < args.burn_seconds:
                t_now, powers = read_powers_mw(gpus)
                t_rel_ms = (time.perf_counter() - burn_t0) * 1000.0

                utils = read_utils(gpus) if args.sample_util else None

                line = [f"[run ] t={t_rel_ms:8.1f} ms"]
                for i, gpu in enumerate(gpus):
                    p_mw = powers[i]
                    line.append(f"GPU{gpu.index}: {p_mw / 1000:.1f} W")

                    if args.sample_util:
                        gu, mu = utils[i]
                        line.append(f"util={gu:3d}% mem={mu:3d}%")

                    if detected_at_ms[i] is None:
                        if p_mw >= thresholds_mw[i]:
                            above_cnt[i] += 1
                            if above_cnt[i] >= max(1, args.confirm_samples):
                                detected_at_ms[i] = t_rel_ms
                        else:
                            above_cnt[i] = 0

                print("  " + "  ".join(line), flush=True)

                elapsed = time.time() - last_sample
                to_sleep = sample_interval - elapsed
                if to_sleep > 0:
                    time.sleep(to_sleep)
                last_sample = time.time()

            start_evt.clear()

            print(f"[info] burn_end_ns   = {time.perf_counter_ns()}\n")

            # Summary per run
            print("Run delay summary (dynamic threshold over last-half idle stats):")
            for i, gpu in enumerate(gpus):
                idle_w = idle_mean_mw[i] / 1000.0
                sigma_w = idle_std_mw[i] / 1000.0
                threshold_w = thresholds_mw[i] / 1000.0
                d = detected_at_ms[i]
                if d is not None:
                    print(
                        f"  GPU {gpu.index}: idle={idle_w:.2f} W, σ={sigma_w:.3f} W, "
                        f"threshold={threshold_w:.2f} W -> delay ≈ {d:.1f} ms"
                    )
                    delays_by_gpu[gpu.index].append(d)
                else:
                    print(
                        f"  GPU {gpu.index}: idle={idle_w:.2f} W, σ={sigma_w:.3f} W, "
                        f"threshold={threshold_w:.2f} W -> delay not detected."
                    )

    finally:
        start_evt.clear()
        stop_evt.set()
        for _gi, p in procs:
            p.join(timeout=3.0)
        try:
            pynvml.nvmlShutdown()
        except Exception:
            pass

    # Stats helpers
    def print_stats(title, values):
        if not values:
            print(f"{title}: no valid measurements")
            return
        vs = sorted(values)
        print(f"{title}: n={len(vs)}")
        print(f"  min   : {vs[0]:.1f} ms")
        print(f"  median: {percentile(vs, 50):.1f} ms")
        print(f"  mean  : {mean(vs):.1f} ms")
        print(f"  p5    : {percentile(vs, 5):.1f} ms")
        print(f"  max   : {vs[-1]:.1f} ms")

    # Per-GPU stats
    print("\n=== Per-GPU delay statistics ===")
    for g in gpus:
        print_stats(f"GPU {g.index} ({g.name})", delays_by_gpu[g.index])

    # Global aggregation
    all_delays = []
    for g in gpus:
        all_delays.extend(delays_by_gpu[g.index])

    if not all_delays:
        print("\nERROR: No valid delay measurements were obtained.")
        sys.exit(1)

    all_sorted = sorted(all_delays)
    d_p5 = percentile(all_sorted, 5)

    print("\n=== Aggregated delay statistics (all runs, all GPUs) ===")
    print_stats("ALL GPUs", all_delays)

    print("\nRecommended (5th percentile) GPU delay:")
    print(f"  {d_p5:.1f} ms")

    # ----------------------------------------------------------------
    # Histograms
    # ----------------------------------------------------------------
    if args.hist:
        os.makedirs(args.hist_outdir, exist_ok=True)
        ts = datetime.now().strftime("%Y%m%d_%H%M%S")

        for g in gpus:
            vals = delays_by_gpu.get(g.index, [])
            if not vals:
                print(f"[hist] GPU {g.index}: no values, skipping")
                continue

            fig = plt.figure()
            plt.hist(vals, bins=max(1, int(args.hist_bins)))
            plt.title(f"GPU {g.index} delay histogram (ms)\n{g.name}")
            plt.xlabel("Delay (ms)")
            plt.ylabel("Count")
            plt.grid(True, which="both", axis="y", linestyle="--", linewidth=0.5)

            # Useful reference lines (no color specified)
            p5 = percentile(sorted(vals), 5)
            med = percentile(sorted(vals), 50)
            if p5 is not None:
                plt.axvline(p5, linestyle="--", linewidth=1.0, label=f"p5={p5:.1f} ms")
            if med is not None:
                plt.axvline(med, linestyle=":", linewidth=1.0, label=f"median={med:.1f} ms")
            plt.legend()

            safe_name = "".join(ch if ch.isalnum() or ch in ("-", "_") else "_" for ch in g.name)
            out = os.path.join(args.hist_outdir, f"delay_hist_gpu{g.index}_{safe_name}_{ts}.png")
            plt.tight_layout()
            plt.savefig(out, dpi=150)
            plt.show()
            plt.close(fig)

            print(f"[hist] wrote {out}")


    sys.exit(0)


if __name__ == "__main__":
    main()
