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
if sys.version_info < (3, 10):
    die(
        "Python >= 3.10 is required",
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
    stress_ok: bool
    stress_max_load1: float


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="Measure Redfish power reaction delay to stress-ng CPU load.")
    p.add_argument("--host", required=True, help="BMC hostname/IP (no scheme), e.g. 192.168.1.10")
    p.add_argument("--user", required=True, help="Redfish username")
    p.add_argument("--password", required=True, help="Redfish password")
    p.add_argument("--path", default=DEFAULT_REDFISH_PATH, help=f"Redfish power resource path (default {DEFAULT_REDFISH_PATH})")

    p.add_argument("--trials", type=int, default=50, help="Number of trials (default: 50)")
    p.add_argument("--cooldown-sec", type=float, default=60.0, help="Cooldown between trials (default: 60)")
    p.add_argument("--baseline-sec", type=float, default=5.0, help="Baseline window length in seconds (default: 5)")
    p.add_argument("--post-stress-grace-sec", type=float, default=10.0, help="Extra time after stress ends to keep polling (default: 10)")

    p.add_argument("--threshold-pct", type=float, default=10.0, help="Threshold above baseline in percent (default: 10)")
    p.add_argument("--consecutive", type=int, default=3, help="Consecutive samples above threshold (default: 3)")

    p.add_argument("--poll-ms", type=int, default=200, help="Initial poll interval in ms (default: 200)")
    p.add_argument("--poll-max-ms", type=int, default=1000, help="Max poll interval in ms (default: 1000)")
    p.add_argument("--jitter-ms", type=int, default=50, help="Uniform random jitter added to sleep in ms (default: 50)")
    p.add_argument("--debug-first-n", type=int, default=5, help="Print debug info for the first N failed Redfish reads per trial (default: 5)")
    p.add_argument("--min-baseline-w", type=float, default=50.0, help="Reject trials with baseline below this (default: 50W). 0W baselines are invalid.")
    p.add_argument("--min-baseline-samples", type=int, default=3, help="Minimum valid baseline samples required (default: 3).")
    p.add_argument("--min-loadavg", type=float, default=1.0, help="Require loadavg(1m) to reach at least this during stress (default: 1.0).")
    p.add_argument("--recommend-quantile", type=float, default=95.0,
                   help="Quantile (in percent) used for recommended delay (default: 95).")
    p.add_argument("--recommend-policy", type=str, default="pXX_plus_halfpoll",
                   choices=["pXX", "pXX_plus_halfpoll", "max"],
                   help="How to compute the single recommended delay. "
                        "pXX uses --recommend-quantile, pXX_plus_halfpoll adds 0.5*median(poll_ms_used), "
                        "max uses max observed delay (most conservative).")

    p.add_argument("--recommend-min-trials", type=int, default=30,
                   help="If fewer valid trials than this, print a warning (default: 30).")

     # stress-ng knobs
    p.add_argument("--stress-sec", type=float, default=10.0, help="stress-ng duration in seconds (default: 10)")
    g = p.add_mutually_exclusive_group(required=True)
    g.add_argument("--cpu-cores", type=int, help="Number of stress-ng CPU workers, e.g. 64")
    g.add_argument("--cpu-pct", type=float, help="Percentage of logical CPUs to use, e.g. 50 for ~half cores")

    p.add_argument("--insecure", action="store_true", default=False, help="Disable TLS verification (default: false)")

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


def redfish_power_w(
    session: requests.Session,
    url: str,
    auth: Tuple[str, str],
    verify_tls: bool,
) -> Tuple[Optional[float], float, float, bool, Optional[int], str]:
    """
    Returns (watts, request_ms, t_end, ok, status_code, err_tag)

    - status_code is None if request failed before getting a response.
    - err_tag is a short diagnostic string. For request exceptions, it includes
      exception class + message, so you can see what is actually failing.
    """
    t_start = time.monotonic()
    try:
        r = session.get(url, auth=auth, timeout=3.0, verify=verify_tls)
        t_end = time.monotonic()
        req_ms = (t_end - t_start) * 1000.0

        sc = r.status_code
        if sc != 200:
            return None, req_ms, t_end, False, sc, f"http_status:{sc}"

        try:
            j = r.json()
        except Exception as e:
            return None, req_ms, t_end, False, sc, f"json_parse:{type(e).__name__}:{e}"

        pc = j.get("PowerControl", None)
        if not isinstance(pc, list) or len(pc) == 0:
            # include top-level keys to quickly spot schema differences
            keys = ",".join(sorted(j.keys())) if isinstance(j, dict) else "non_dict_json"
            return None, req_ms, t_end, False, sc, f"missing_PowerControl:keys={keys}"

        w = pc[0].get("PowerConsumedWatts", None)
        if w is None:
            # include keys inside PowerControl[0] to see what vendor provides
            pc0_keys = ",".join(sorted(pc[0].keys())) if isinstance(pc[0], dict) else "non_dict_pc0"
            return None, req_ms, t_end, False, sc, f"missing_PowerConsumedWatts:pc0_keys={pc0_keys}"

        try:
            w = float(w)
        except Exception as e:
            return None, req_ms, t_end, False, sc, f"w_not_float:{type(e).__name__}:{e}"

        if math.isnan(w) or w < 0:
            return None, req_ms, t_end, False, sc, "w_nan_or_neg"

        return w, req_ms, t_end, True, sc, ""
    except Exception as e:
        t_end = time.monotonic()
        req_ms = (t_end - t_start) * 1000.0
        return None, req_ms, t_end, False, None, f"request_exc:{type(e).__name__}:{e}"


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
    debug_first_n: int = 0,
) -> bool:
    """
    Wait until power stays <= target_w for hold_sec continuously.
    Returns True if achieved, False if timed out.
    """
    t_deadline = time.monotonic() + timeout_sec
    t_ok_start: Optional[float] = None

    while time.monotonic() < t_deadline:
        w, _req_ms, _t_end, ok, _sc, _tag = redfish_power_w(session, url, auth, verify_tls)
        if (not ok) and debug_first_n > 0:
            print(
                f"[debug cooldown] ok={ok} status={_sc} tag={_tag} req_ms={_req_ms:.0f}ms",
                flush=True,
            )
            debug_first_n -= 1
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
    debug_first_n: int = 0,
    min_baseline_w: float = 50.0,
    min_baseline_samples: int = 3,
    min_loadavg: float = 1.0,
) -> TrialResult:
    """
    Measures Redfish "reaction delay" to a CPU step.
    Adds:
      - Baseline integrity gates (reject 0W glitches)
      - Stress validation via local loadavg(1m) max during stress window
    """
    poll_ms_used = poll_ms

    # --- Baseline collection ---
    baseline_vals: List[float] = []
    req_ms_list: List[float] = []
    req_count = 0
    err_count = 0
    recent_ok: List[bool] = []
    recent_req_ms: List[float] = []
    debug_left = int(debug_first_n) if debug_first_n is not None else 0

    baseline_deadline = time.monotonic() + baseline_sec
    while time.monotonic() < baseline_deadline:
        w, req_ms, _t_end, ok, sc, tag = redfish_power_w(session, url, auth, verify_tls)

        if (not ok or w is None) and debug_left > 0:
            print(f"[debug baseline] status={sc} tag={tag} req_ms={req_ms:.0f}ms", flush=True)
            debug_left -= 1

        req_ms_list.append(req_ms)
        req_count += 1
        if ok and w is not None and math.isfinite(w) and w >= min_baseline_w:
            baseline_vals.append(w)
        else:
            # Treat too-small / bogus baseline values (notably 0W) as invalid samples
            err_count += 1
            if debug_left > 0:
                print(
                    f"[debug baseline] status={sc} tag={tag} w={w} req_ms={req_ms:.0f}ms",
                    flush=True,
                )
                debug_left -= 1


        # Keep approx poll period stable (account for request time)
        sleep_s = max(0.0, (poll_ms_used / 1000.0) - (req_ms / 1000.0))
        sleep_with_jitter(sleep_s, jitter_ms)

        # quick backoff if we are clearly over-polling / failing
        if req_count >= 5:
            recent_err_rate = err_count / max(1, req_count)
            if recent_err_rate >= 0.4 and poll_ms_used < poll_max_ms:
                poll_ms_used = min(poll_max_ms, int(round(poll_ms_used * 1.5)))

    # --- Baseline integrity gates ---
    p95 = float(np.percentile(req_ms_list, 95)) if req_ms_list else float("nan")

    if len(baseline_vals) < int(min_baseline_samples):
        return TrialResult(
            delay_sec=None,
            baseline_w=float("nan"),
            threshold_w=float("nan"),
            poll_ms_used=poll_ms_used,
            req_count=req_count,
            err_count=err_count,
            req_ms_p95=p95,
            stress_ok=False,
            stress_max_load1=0.0,
        )

    baseline_w = float(np.median(np.array(baseline_vals, dtype=float)))

    # Reject bogus baselines (notably 0W sensor glitches)
    if (not math.isfinite(baseline_w)) or (baseline_w < float(min_baseline_w)):
        return TrialResult(
            delay_sec=None,
            baseline_w=baseline_w,
            threshold_w=float("nan"),
            poll_ms_used=poll_ms_used,
            req_count=req_count,
            err_count=err_count,
            req_ms_p95=p95,
            stress_ok=False,
            stress_max_load1=0.0,
        )

    threshold_w = baseline_w * (1.0 + (threshold_pct / 100.0))

    # --- Start stress-ng (t0) ---
    cmd = [
        "stress-ng",
        "--cpu", str(workers),
        "--timeout", f"{stress_sec:.3f}s",
        "--metrics-brief",
        "--quiet",
    ]

    p = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    t0 = time.monotonic()  # IMPORTANT: timestamp AFTER launch

    # Quick sanity: confirm stress-ng is alive right after launch
    time.sleep(0.2)
    if p.poll() is not None:
        print(f"[debug] stress-ng exited immediately with rc={p.returncode}", flush=True)
    else:
        print(f"[debug] stress-ng running (pid={p.pid}) (workers={str(workers)})", flush=True)

    # --- Poll until threshold crossed (debounced) or until deadline ---
    above_count = 0
    delay_sec: Optional[float] = None
    first_cross_t: Optional[float] = None

    deadline = t0 + stress_sec + post_stress_grace_sec

    # Stress validation
    stress_max_load1 = 0.0

    while time.monotonic() < deadline:
        # Update stress validation signal (local machine running this script)
        try:
            l1 = os.getloadavg()[0]
            if l1 > stress_max_load1:
                stress_max_load1 = l1
        except Exception:
            pass

        w, req_ms, t_end, ok, sc, tag = redfish_power_w(session, url, auth, verify_tls)
        req_ms_list.append(req_ms)
        req_count += 1

        recent_ok.append(ok and (w is not None))
        recent_req_ms.append(req_ms)
        if len(recent_ok) > 20:
            recent_ok.pop(0)
            recent_req_ms.pop(0)

        if ok and w is not None:
            # Timestamp observation near the middle of the HTTP request
            obs_t = t_end - (req_ms / 1000.0) / 2.0

            if w >= threshold_w:
                if above_count == 0:
                    first_cross_t = obs_t
                above_count += 1
                if above_count >= consecutive:
                    delay_sec = (first_cross_t - t0) if first_cross_t is not None else (obs_t - t0)
                    break
            else:
                above_count = 0
                first_cross_t = None
        else:
            err_count += 1
            above_count = 0
            first_cross_t = None

        # backoff rule: if error rate or latency is too high, increase poll interval
        if len(recent_ok) >= 10:
            recent_err_rate = 1.0 - (sum(1 for x in recent_ok if x) / len(recent_ok))
            recent_p95 = float(np.percentile(recent_req_ms, 95))
            if (recent_err_rate > 0.10 or recent_p95 > (0.8 * poll_ms_used)) and poll_ms_used < poll_max_ms:
                poll_ms_used = min(poll_max_ms, int(round(poll_ms_used * 1.5)))

        # keep approx poll period stable
        sleep_s = max(0.0, (poll_ms_used / 1000.0) - (req_ms / 1000.0))
        sleep_with_jitter(sleep_s, jitter_ms)

    # Ensure stress-ng is gone (should be by timeout, but be safe)
    try:
        p.wait(timeout=1.0)
    except subprocess.TimeoutExpired:
        p.terminate()
        try:
            p.wait(timeout=1.0)
        except subprocess.TimeoutExpired:
            p.kill()

    # Final diagnostics
    p95 = float(np.percentile(req_ms_list, 95)) if req_ms_list else float("nan")
    stress_ok = (stress_max_load1 >= float(min_loadavg))

    return TrialResult(
        delay_sec=delay_sec,
        baseline_w=baseline_w,
        threshold_w=threshold_w,
        poll_ms_used=poll_ms_used,
        req_count=req_count,
        err_count=err_count,
        req_ms_p95=p95,
        stress_ok=stress_ok,
        stress_max_load1=stress_max_load1,
    )

def compute_recommended_delay_sec(
    delays: np.ndarray,
    poll_used_ms: List[int],
    policy: str,
    quantile_pct: float,
) -> float:
    if delays.size == 0:
        return float("nan")

    q = float(np.percentile(delays, quantile_pct))
    maxd = float(np.max(delays))

    # Sampling quantization margin: half the typical poll period
    halfpoll = 0.5 * (float(statistics.median(poll_used_ms)) / 1000.0) if poll_used_ms else 0.0

    if policy == "max":
        return maxd

    if policy == "pXX":
        return q

    if policy == "pXX_plus_halfpoll":
        return q + halfpoll

    # default fallback
    return q + halfpoll


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
                target_w=max(args.min_baseline_w, prev_baseline_w) * 1.05,
                hold_sec=10.0,
                timeout_sec=120.0,
                poll_ms=500,
                debug_first_n=args.debug_first_n,
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
            debug_first_n=args.debug_first_n,
            post_stress_grace_sec=args.post_stress_grace_sec,
            workers=workers,
            min_baseline_w=args.min_baseline_w,
            min_baseline_samples=args.min_baseline_samples,
            min_loadavg=args.min_loadavg,
        )
        results.append(tr)
        # Only carry forward a baseline if it is clearly valid (avoid 0W poisoning)
        if (
            tr.baseline_w is not None
            and math.isfinite(tr.baseline_w)
            and tr.baseline_w >= args.min_baseline_w
        ):
            prev_baseline_w = tr.baseline_w
        else:
            prev_baseline_w = None

        det = "OK" if tr.delay_sec is not None else "MISS"
        err_rate = tr.err_count / max(1, tr.req_count)
        if tr.delay_sec is not None:
            print(f"Trial {i+1:03d}/{args.trials}: OK  | delay={tr.delay_sec:.3f}s")
        else:
            print(f"Trial {i+1:03d}/{args.trials}: MISS")
        print(
            f"  baseline={tr.baseline_w:.1f}W  thr={tr.threshold_w:.1f}W"
            f"  poll={tr.poll_ms_used}ms  req_p95={tr.req_ms_p95:.0f}ms  err_rate={err_rate:.1%}"
        )

        if i + 1 < args.trials:
            time.sleep(args.cooldown_sec)

    # --- Stats ---
    # --- Stats (VALID trials only) ---
    valid = [
        r for r in results
        if r.delay_sec is not None
        and math.isfinite(r.delay_sec)
        and r.stress_ok
        and math.isfinite(r.baseline_w)
        and r.baseline_w >= getattr(args, "min_baseline_w", 50.0)
    ]

    detected = len([r for r in results if r.delay_sec is not None])
    misses = len(results) - detected
    invalid_baseline = sum(
        1 for r in results
        if (not math.isfinite(r.baseline_w)) or r.baseline_w < getattr(args, "min_baseline_w", 50.0)
    )
    stress_failed = sum(1 for r in results if not r.stress_ok)

    print("\n=== Summary ===")
    print(f"Detected (raw): {detected}/{len(results)} (misses: {misses})")
    print(f"Valid trials:   {len(valid)}/{len(results)}")
    print(f"Invalid baseline trials: {invalid_baseline}")
    print(f"Stress validation failed: {stress_failed}")

    if not valid:
        print("No valid detections to summarize.")
        print("Likely causes: baseline glitches, stress not applied, or threshold too high.")
        return

    delays = np.array([r.delay_sec for r in valid], dtype=float)
    poll_used = [r.poll_ms_used for r in valid]

    mean = float(delays.mean())
    std = float(delays.std(ddof=1)) if len(delays) > 1 else 0.0
    med = float(np.median(delays))
    p90 = float(np.percentile(delays, 90))
    p95 = float(np.percentile(delays, 95))
    p99 = float(np.percentile(delays, 99)) if len(delays) >= 10 else float("nan")
    dmax = float(np.max(delays))

    print(f"\nValid delay stats (seconds):")
    print(f"Mean:   {mean:.3f}s")
    print(f"Std:    {std:.3f}s")
    print(f"Median: {med:.3f}s")
    print(f"p90:    {p90:.3f}s")
    print(f"p95:    {p95:.3f}s")
    if not math.isnan(p99):
        print(f"p99:    {p99:.3f}s")
    print(f"Max:    {dmax:.3f}s")

    # --- Single conservative recommended delay ---
    qpct = float(getattr(args, "recommend_quantile", 95.0))
    policy = str(getattr(args, "recommend_policy", "pXX_plus_halfpoll"))

    rec = compute_recommended_delay_sec(
        delays=delays,
        poll_used_ms=poll_used,
        policy=policy,
        quantile_pct=qpct,
    )


    # Also compute the two main candidates for transparency:
    halfpoll = 0.5 * (float(statistics.median(poll_used)) / 1000.0) if poll_used else 0.0
    qval = float(np.percentile(delays, qpct))

    print("\n=== Recommended single delay (conservative) ===")
    print(f"Policy: {policy}  (quantile={qpct:.1f}%)")
    print(f"Quantile value: {qval:.3f}s")
    print(f"Half-poll margin (median poll_ms_used): {halfpoll:.3f}s")
    print(f"Recommended delay: {rec:.3f}s")

    # Warn if too few trials
    min_trials = int(getattr(args, "recommend_min_trials", 30))
    if len(valid) < min_trials:
        print(f"WARNING: only {len(valid)} valid trials (< {min_trials}). "
              f"Recommended delay may be unstable; run more trials.")

    # Diagnostics on polling health (valid trials)
    req_p95s = [r.req_ms_p95 for r in valid if not math.isnan(r.req_ms_p95)]
    err_rates = [r.err_count / max(1, r.req_count) for r in valid]

    print("\n=== Diagnostics (valid trials) ===")
    print(f"poll_ms_used: min={min(poll_used)} max={max(poll_used)} median={statistics.median(poll_used)}")
    if req_p95s:
        print(f"req_ms_p95:   median={statistics.median(req_p95s):.0f}ms  max={max(req_p95s):.0f}ms")
    print(f"err_rate:     median={statistics.median(err_rates):.1%}  max={max(err_rates):.1%}")

    # --- Histogram (valid trials only) ---
    arr = delays
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
