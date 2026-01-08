#!/usr/bin/env python3
"""
Measure Redfish power "reaction delay" to a CPU workload step (stress-ng).

Definition of "reaction":
- baseline = mean power over BASELINE_SEC seconds before starting stress-ng
- threshold = baseline * (1 + THRESHOLD_PCT/100)
- delay = first time we observe power >= threshold for CONSECUTIVE samples, minus t0
- t0 = time.monotonic() timestamp immediately after launching stress-ng

Notes:
- Poll interval starts at --poll-ms (default 200 ms) and can back off up to 1000 ms.
- No CSV logging (by request). The script prints stats and writes a histogram PNG.
"""

from __future__ import annotations

import argparse
import math
import os
import random
import statistics
import subprocess
import time
from dataclasses import dataclass
from typing import Optional, Tuple, List
from pathlib import Path
from datetime import datetime

import requests
import numpy as np
import matplotlib.pyplot as plt
import sys
import shutil
import warnings
from urllib3.exceptions import InsecureRequestWarning

warnings.simplefilter("ignore", InsecureRequestWarning)


DEFAULT_REDFISH_PATH = "/redfish/v1/Chassis/1/Power"
SCRIPT_DIR = Path(__file__).resolve().parent

def die(msg: str, hint: str | None = None) -> None:
    print(f"\nERROR: {msg}", file=sys.stderr)
    if hint:
        print(f"HINT:  {hint}", file=sys.stderr)
    sys.exit(1)


# ---- Python version check ----------------------------------------------------
if sys.version_info < (3, 9):
    die(
        "Python >= 3.9 is required",
        "Upgrade Python or run with a newer interpreter",
    )


# ---- External dependency check ----------------------------------------------
if shutil.which("stress-ng") is None:
    die(
        "stress-ng is not installed or not in PATH",
        "Install via your package manager, e.g.: sudo apt install stress-ng",
    )


# ---- Python module checks -----------------------------------------------------
try:
    import requests
except ImportError:
    die(
        "Python module 'requests' is not installed",
        "Install with: pip install requests",
    )

try:
    import numpy as np
except ImportError:
    die(
        "Python module 'numpy' is not installed",
        "Install with: pip install numpy",
    )

try:
    import matplotlib.pyplot as plt
except ImportError:
    die(
        "Python module 'matplotlib' is not installed",
        "Install with: pip install matplotlib",
    )

@dataclass
class TrialResult:
    delay_sec: Optional[float]          # None if not detected
    baseline_w: float
    threshold_w: float
    poll_ms_used: int
    req_count: int
    err_count: int
    req_ms_p95: float


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Measure Redfish power reaction delay to stress-ng CPU load.")
    p.add_argument("--host", required=True, help="BMC hostname/IP (no scheme), e.g. 192.168.1.10")
    p.add_argument("--user", required=True, help="Redfish username")
    p.add_argument("--password", required=True, help="Redfish password")
    p.add_argument("--path", default=DEFAULT_REDFISH_PATH, help=f"Redfish power resource path (default {DEFAULT_REDFISH_PATH})")

    p.add_argument("--trials", type=int, default=50, help="Number of trials (default: 50)")
    p.add_argument("--cooldown-sec", type=float, default=60.0, help="Cooldown between trials (default: 30)")
    p.add_argument("--baseline-sec", type=float, default=5.0, help="Baseline window length in seconds (default: 5)")
    p.add_argument("--post-stress-grace-sec", type=float, default=10.0, help="Extra time after stress ends to keep polling (default: 10)")

    p.add_argument("--threshold-pct", type=float, default=10.0, help="Threshold above baseline in percent (default: 10)")
    p.add_argument("--consecutive", type=int, default=3, help="Consecutive samples above threshold (default: 3)")

    p.add_argument("--poll-ms", type=int, default=200, help="Initial poll interval in ms (default: 200)")
    p.add_argument("--poll-max-ms", type=int, default=1000, help="Max poll interval in ms (default: 1000)")
    p.add_argument("--jitter-ms", type=int, default=50, help="Uniform random jitter added to sleep in ms (default: 50)")

    # stress-ng knobs
    p.add_argument("--stress-sec", type=float, default=10.0, help="stress-ng duration in seconds (default: 10)")
    g = p.add_mutually_exclusive_group(required=True)
    g.add_argument("--cpu-cores", type=int, help="Number of stress-ng CPU workers, e.g. 64")
    g.add_argument("--cpu-pct", type=float, help="Percentage of logical CPUs to use, e.g. 50 for ~half cores")

    p.add_argument("--insecure", action="store_true", default=True, help="Disable TLS verification (default: true)")
    p.add_argument("--secure", dest="insecure", action="store_false", help="Enable TLS verification")

    p.add_argument("--hist-out", default="delay_hist_redfish.png", help="Histogram output file (default: redfish_delay_hist.png)")
    p.add_argument("--seed", type=int, default=0, help="Random seed (0 = time-based)")

    return p.parse_args()


def compute_workers(cpu_cores: Optional[int], cpu_pct: Optional[float]) -> int:
    if cpu_cores is not None:
        return max(1, cpu_cores)
    assert cpu_pct is not None
    n = os.cpu_count() or 1
    workers = int(round((cpu_pct / 100.0) * n))
    return max(1, min(n, workers))


def redfish_power_w(session: requests.Session, url: str, auth: Tuple[str, str], verify_tls: bool) -> Tuple[Optional[float], float, bool]:
    """
    Returns (watts, request_ms, ok)
    ok=False for HTTP errors / parse errors / missing value.
    """
    t_start = time.monotonic()
    try:
        r = session.get(url, auth=auth, timeout=3.0, verify=verify_tls)
        req_ms = (time.monotonic() - t_start) * 1000.0
        if r.status_code != 200:
            return None, req_ms, False
        j = r.json()
        # PowerControl[0].PowerConsumedWatts
        pc = j.get("PowerControl", None)
        if not isinstance(pc, list) or len(pc) == 0:
            return None, req_ms, False
        w = pc[0].get("PowerConsumedWatts", None)
        if w is None:
            return None, req_ms, False
        try:
            w = float(w)
        except Exception:
            return None, req_ms, False
        if math.isnan(w) or w < 0:
            return None, req_ms, False
        return w, req_ms, True
    except Exception:
        req_ms = (time.monotonic() - t_start) * 1000.0
        return None, req_ms, False


def sleep_with_jitter(base_sec: float, jitter_ms: int) -> None:
    if base_sec <= 0:
        return
    jitter_sec = 0.0
    if jitter_ms > 0:
        jitter_sec = random.uniform(-jitter_ms, jitter_ms) / 1000.0
    time.sleep(max(0.0, base_sec + jitter_sec))

def wait_for_power_below(
    session: requests.Session,
    url: str,
    auth: Tuple[str, str],
    verify_tls: bool,
    target_w: float,
    hold_sec: float,
    timeout_sec: float,
    poll_ms: int,
) -> bool:
    """
    Wait until power stays <= target_w for hold_sec continuously.
    Returns True if achieved, False if timed out.
    """
    t_deadline = time.monotonic() + timeout_sec
    t_ok_start: Optional[float] = None

    while time.monotonic() < t_deadline:
        w, _, ok = redfish_power_w(session, url, auth, verify_tls)
        now = time.monotonic()
        if ok and w is not None and w <= target_w:
            if t_ok_start is None:
                t_ok_start = now
            if (now - t_ok_start) >= hold_sec:
                return True
        else:
            t_ok_start = None
        time.sleep(poll_ms / 1000.0)
    return False



def run_one_trial(
    session: requests.Session,
    url: str,
    auth: Tuple[str, str],
    verify_tls: bool,
    poll_ms: int,
    poll_max_ms: int,
    jitter_ms: int,
    baseline_sec: float,
    threshold_pct: float,
    consecutive: int,
    stress_sec: float,
    post_stress_grace_sec: float,
    workers: int,
) -> TrialResult:
    poll_ms_used = poll_ms

    # Optional: ensure we've cooled down from previous run.
    # If previous baseline exists, main() can pass it in; simplest version uses a fixed target.
    # For now, do nothing here; see Change 4 for wiring previous baseline.

    # --- Baseline collection (mean over baseline_sec) ---
    baseline_vals: List[float] = []
    req_ms_list: List[float] = []
    req_count = 0
    err_count = 0
    recent_ok: List[bool] = []
    recent_req_ms: List[float] = []

    baseline_deadline = time.monotonic() + baseline_sec
    while time.monotonic() < baseline_deadline:
        w, req_ms, ok = redfish_power_w(session, url, auth, verify_tls)
        req_ms_list.append(req_ms)
        req_count += 1
        if ok and w is not None:
            baseline_vals.append(w)
        else:
            err_count += 1
        sleep_with_jitter(poll_ms_used / 1000.0, jitter_ms)

        # quick backoff if we are clearly over-polling / failing
        if req_count >= 5:
            recent_err_rate = err_count / max(1, req_count)
            if recent_err_rate >= 0.4 and poll_ms_used < poll_max_ms:
                poll_ms_used = min(poll_max_ms, int(round(poll_ms_used * 1.5)))

    if not baseline_vals:
        # baseline failed; return a "no detection" trial with diagnostics
        p95 = float(np.percentile(req_ms_list, 95)) if req_ms_list else float("nan")
        return TrialResult(
            delay_sec=None,
            baseline_w=float("nan"),
            threshold_w=float("nan"),
            poll_ms_used=poll_ms_used,
            req_count=req_count,
            err_count=err_count,
            req_ms_p95=p95,
        )

    baseline_w = float(np.median(np.array(baseline_vals, dtype=float)))
    threshold_w = baseline_w * (1.0 + (threshold_pct / 100.0))

    # --- Start stress-ng (t0) ---
    # Use a tight command with a fixed timeout (stress_sec).
    cmd = [
        "stress-ng",
        "--cpu", str(workers),
        "--timeout", f"{stress_sec:.3f}s",
        "--metrics-brief",
        "--quiet",
    ]
    p = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    t0 = time.monotonic()

    # --- Poll until threshold crossed (for 'consecutive' samples) or until stress ends + grace ---
    above_count = 0
    delay_sec: Optional[float] = None

    # Some systems show Redfish values on a coarse tick; give a small grace after stress end
    deadline = t0 + stress_sec + post_stress_grace_sec  # requires passing arg, see note below

    # Reset per-phase request latency stats (baseline already collected, but keep all for diagnostics)
    while time.monotonic() < deadline:
        w, req_ms, ok = redfish_power_w(session, url, auth, verify_tls)
        req_ms_list.append(req_ms)
        req_count += 1

        recent_ok.append(ok and (w is not None))
        recent_req_ms.append(req_ms)
        if len(recent_ok) > 20:
            recent_ok.pop(0)
            recent_req_ms.pop(0)

        first_cross_t: Optional[float] = None

        # inside the loop:
        if ok and w is not None:
            now = time.monotonic()  # timestamp for *this* observation point (post-request)
            if w >= threshold_w:
                if above_count == 0:
                    first_cross_t = now
                above_count += 1
                if above_count >= consecutive:
                    # Report FIRST crossing time, not the Nth confirmation sample
                    delay_sec = (first_cross_t - t0) if first_cross_t is not None else (now - t0)
                    break
            else:
                above_count = 0
                first_cross_t = None
        else:
            err_count += 1
            above_count = 0
            first_cross_t = None


        # backoff rule: if error rate or latency is too high, increase poll interval (cap poll_max_ms)
        # - if we get 3 errors in the last ~10 reqs, back off
        if len(recent_ok) >= 10:
            recent_err_rate = 1.0 - (sum(1 for x in recent_ok if x) / len(recent_ok))
            recent_p95 = float(np.percentile(recent_req_ms, 95))
            if (recent_err_rate > 0.10 or recent_p95 > (0.8 * poll_ms_used)) and poll_ms_used < poll_max_ms:
                poll_ms_used = min(poll_max_ms, int(round(poll_ms_used * 1.5)))

        sleep_with_jitter(poll_ms_used / 1000.0, jitter_ms)

    # Ensure stress-ng is gone (should be by timeout, but be safe)
    try:
        p.wait(timeout=1.0)
    except subprocess.TimeoutExpired:
        p.terminate()
        try:
            p.wait(timeout=1.0)
        except subprocess.TimeoutExpired:
            p.kill()

    p95 = float(np.percentile(req_ms_list, 95)) if req_ms_list else float("nan")
    return TrialResult(
        delay_sec=delay_sec,
        baseline_w=baseline_w,
        threshold_w=threshold_w,
        poll_ms_used=poll_ms_used,
        req_count=req_count,
        err_count=err_count,
        req_ms_p95=p95,
    )


def main() -> None:
    args = parse_args()
    if args.seed == 0:
        random.seed()
    else:
        random.seed(args.seed)

    workers = compute_workers(args.cpu_cores, args.cpu_pct)
    url = f"https://{args.host}{args.path}"
    auth = (args.user, args.password)
    verify_tls = not args.insecure

    print("=== Redfish delay measurement ===")
    print(f"URL:          {url}")
    print(f"Trials:       {args.trials}")
    print(f"Baseline:     {args.baseline_sec:.1f}s mean")
    print(f"Threshold:    +{args.threshold_pct:.1f}% over baseline")
    print(f"Consecutive:  {args.consecutive}")
    print(f"Stress:       {args.stress_sec:.1f}s, stress-ng workers={workers}")
    print(f"Polling:      start {args.poll_ms}ms, max {args.poll_max_ms}ms, jitter ±{args.jitter_ms}ms")
    print(f"Cooldown:     {args.cooldown_sec:.1f}s")
    print("")

    session = requests.Session()
    prev_baseline_w: Optional[float] = None

    results: List[TrialResult] = []
    for i in range(args.trials):
        
        if prev_baseline_w is not None:
            # Wait until power returns near previous baseline (+5%) for 10s, up to 120s
            wait_for_power_below(
                session=session,
                url=url,
                auth=auth,
                verify_tls=verify_tls,
                target_w=prev_baseline_w * 1.05,
                hold_sec=10.0,
                timeout_sec=120.0,
                poll_ms=500,
            )

        tr = run_one_trial(
            session=session,
            url=url,
            auth=auth,
            verify_tls=verify_tls,
            poll_ms=args.poll_ms,
            poll_max_ms=args.poll_max_ms,
            jitter_ms=args.jitter_ms,
            baseline_sec=args.baseline_sec,
            threshold_pct=args.threshold_pct,
            consecutive=args.consecutive,
            stress_sec=args.stress_sec,
            post_stress_grace_sec=args.post_stress_grace_sec,
            workers=workers,
        )
        results.append(tr)
        if not math.isnan(tr.baseline_w):
            prev_baseline_w = tr.baseline_w

        det = "OK" if tr.delay_sec is not None else "MISS"
        err_rate = tr.err_count / max(1, tr.req_count)
        print(
            f"Trial {i+1:03d}/{args.trials}: {det}"
            f" | delay={tr.delay_sec:.3f}s" if tr.delay_sec is not None else
            f"Trial {i+1:03d}/{args.trials}: {det}"
        )
        print(
            f"  baseline={tr.baseline_w:.1f}W  thr={tr.threshold_w:.1f}W"
            f"  poll={tr.poll_ms_used}ms  req_p95={tr.req_ms_p95:.0f}ms  err_rate={err_rate:.1%}"
        )

        if i + 1 < args.trials:
            time.sleep(args.cooldown_sec)

    # --- Stats ---
    delays = [r.delay_sec for r in results if r.delay_sec is not None and not math.isnan(r.delay_sec)]
    misses = len(results) - len(delays)

    print("\n=== Summary ===")
    print(f"Detected: {len(delays)}/{len(results)} (misses: {misses})")
    if not delays:
        print("No detections. Consider lowering threshold-pct, increasing stress intensity, or relaxing consecutive.")
        return

    arr = np.array(delays, dtype=float)
    mean = float(arr.mean())
    std = float(arr.std(ddof=1)) if len(arr) > 1 else 0.0
    med = float(np.median(arr))
    p90 = float(np.percentile(arr, 90))
    p95 = float(np.percentile(arr, 95))
    p99 = float(np.percentile(arr, 99)) if len(arr) >= 10 else float("nan")

    print(f"Mean:   {mean:.3f}s")
    print(f"Std:    {std:.3f}s")
    print(f"Median: {med:.3f}s")
    print(f"p90:    {p90:.3f}s")
    print(f"p95:    {p95:.3f}s")
    if not math.isnan(p99):
        print(f"p99:    {p99:.3f}s")

    # Diagnostics on polling health
    req_p95s = [r.req_ms_p95 for r in results if not math.isnan(r.req_ms_p95)]
    poll_used = [r.poll_ms_used for r in results]
    err_rates = [r.err_count / max(1, r.req_count) for r in results]

    print("\n=== Diagnostics ===")
    print(f"poll_ms_used: min={min(poll_used)} max={max(poll_used)} median={statistics.median(poll_used)}")
    print(f"req_ms_p95:   median={statistics.median(req_p95s):.0f}ms  max={max(req_p95s):.0f}ms")
    print(f"err_rate:     median={statistics.median(err_rates):.1%}  max={max(err_rates):.1%}")

    # --- Histogram ---
    plt.figure()

    bin_width = 0.1  # seconds (100 ms). Try 0.05 for 50 ms.
    dmin = float(arr.min())
    dmax = float(arr.max())

    # Create bin edges that align nicely
    start = math.floor(dmin / bin_width) * bin_width
    end = math.ceil(dmax / bin_width) * bin_width
    bins = np.arange(start, end + bin_width, bin_width)

    # --- Histogram ---
    plt.figure()

    bin_width = 0.1  # seconds (100 ms). Try 0.05 for 50 ms.
    dmin = float(arr.min())
    dmax = float(arr.max())

    # Create bin edges that align nicely
    start = math.floor(dmin / bin_width) * bin_width
    end = math.ceil(dmax / bin_width) * bin_width
    bins = np.arange(start, end + bin_width, bin_width)

    # Base output path (saved next to this script, regardless of CWD)
    hist_path = SCRIPT_DIR / args.hist_out

    # Timestamped output file
    ts = datetime.now().strftime("%Y%m%d_%H%M%S")
    out_path = hist_path.with_name(f"{hist_path.stem}_{ts}{hist_path.suffix}")

    plt.hist(arr, bins=bins)
    plt.xlabel("Delay (seconds)")
    plt.ylabel("Count")
    plt.title(f"Redfish reaction delay distribution (bin={bin_width*1000:.0f} ms)")
    plt.tight_layout()
    plt.savefig(out_path, dpi=150)
    print(f"\nHistogram written to: {out_path}")


if __name__ == "__main__":
    main()
