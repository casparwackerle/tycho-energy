# Tycho

Tycho is a **research fork of Kepler v0.9** (Kubernetes Efficient Power Level Exporter) that focuses on
**accuracy-first container-level energy attribution** in Kubernetes.

Unlike production-oriented energy monitoring tools, Tycho prioritizes
temporal fidelity, explicit modelling assumptions, and strict energy conservation.
It is designed to support research-grade analysis of CPU, GPU, and system-level
energy consumption under concurrent, heterogeneous, and short-lived workloads,
rather than low-overhead, best-effort monitoring.


## Repository structure (Tycho vs Kepler)

This repository combines original Tycho code with inherited upstream Kepler framework components.

- [`internal/tycho/`](internal/tycho/) contains the **Tycho implementation** and the primary contribution of this work.
- [`pkg/`](pkg/) contains **upstream Kepler framework code**, largely retained to avoid risky refactoring shortly before thesis submission. Only sporadic parts of this code are still used, mainly for compatibility and integration with existing collector and exporter paths.

For thesis review and technical understanding of Tycho, readers should start with [`internal/tycho/`](internal/tycho/) and the documentation in [`the architecture chapter of the thesis`](https://github.com/casparwackerle/PowerStack/tree/main/thesis/MT/).


## Project overview

- **Motivation:** Upstream Kepler is evolving toward reduced privileges and lower overhead,
  de-emphasizing high-frequency eBPF-based collection to improve deployability at scale.
  While sensible for broad adoption, this shift limits temporal resolution and attribution
  fidelity, which are critical for research and validation. Tycho explores this accuracy
  frontier by retaining high-frequency historical measurement data, decoupling metric
  collection from analysis time, explicitly modelling observation delay, and introducing
  domain-specific energy models for CPUs, GPUs, and system-level telemetry.
- **Context:** This repository underpins a **Master’s thesis** and will change as the work progresses. The thesis deadline is January 31st, 2026.
- **Status:** Active research fork; APIs, flags, and configuration **may change without notice**.

## Core design principles

Tycho is guided by a small set of explicit design principles:

- **Accuracy over deployability:** Measurement fidelity and temporal correctness are
  prioritized over minimal privileges or lowest overhead.
- **Event-time attribution:** Metrics are interpreted according to when underlying
  behavior occurred, not merely when values were observed.
- **Historical buffering:** High-frequency raw measurements are retained in bounded
  buffers, enabling post hoc alignment, correlation, and re-aggregation.
- **Asynchronous metric collection:** Each telemetry source is polled independently
  at a source-appropriate frequency, preserving native semantics.
- **Strict conservation:** All observed energy is accounted for explicitly, either
  as workload-attributed or residual energy, with no silent redistribution.
- **Uncertainty-aware modelling:** Tycho does not imply a unique ground truth and
  exposes unexplained or ambiguous energy explicitly.

These principles distinguish Tycho from window-delta-based attribution approaches
and form the conceptual foundation of the system.

## Key contributions and design innovations

Tycho differs from existing container-level energy monitoring tools through a
set of explicit, research-driven design choices aimed at maximizing attribution
fidelity rather than minimizing overhead. The central contributions of Tycho are:

- **Historically buffered, high-frequency metric retention**  
  Instead of relying solely on start–end deltas over fixed analysis windows,
  Tycho retains fine-grained, high-frequency raw measurement data in bounded
  historical buffers. Metrics are collected at source-appropriate intervals
  (tens to hundreds of milliseconds) and preserved beyond a single attribution
  window. This enables post hoc alignment, correlation, and re-aggregation of
  heterogeneous energy and utilization signals, and allows short-lived or
  sequential workloads within the same window to be distinguished.

- **Independent, asynchronously timed metric collectors**  
  Each telemetry source is polled independently at a frequency suited to its
  measurement semantics. Quasi-instantaneous sources such as RAPL and eBPF are
  sampled at high frequency, while internally buffered sources such as NVML and
  Redfish are queried in a manner that maximizes information content while
  avoiding redundant polling. This preserves native temporal structure and
  avoids artificial synchronization at collection time.

- **Explicit modelling of metric delay and temporal uncertainty**  
  Tycho incorporates metric-specific observation delay directly into its
  temporal alignment and attribution logic. By accounting for stable and
  variable delays across telemetry sources, Tycho aligns metrics based on
  inferred real-world behavior rather than raw observation timestamps, reducing
  misattribution caused by asynchronous publication.

- **Phase-aligned GPU telemetry polling and composite GPU energy modelling**  
  GPU power and energy metrics are generated according to device-internal
  publication cycles. Tycho estimates these cycles and phase-aligns polling
  operations accordingly, improving temporal accuracy without excessive
  overhead. Multiple heterogeneous NVML signals (instant power, averaged power,
  cumulative energy) are interpreted according to their semantics and fused into
  a composite GPU energy model with higher temporal resolution than any single
  raw metric.

- **System-level energy refinement via multi-source fusion**  
  Coarse, delayed system-level energy telemetry (e.g., Redfish) is refined using
  higher-frequency subsystem signals such as CPU, GPU, and kernel-level
  utilization metrics. Historical buffering enables interpolation between
  system-level samples and continuous model adjustment, producing a temporally
  finer system energy representation suitable for workload-level attribution.

- **Strict conservation and explicit residual energy handling**  
  All observed energy is accounted for explicitly. Energy that cannot be
  attributed to workloads or domains appears as residual energy rather than
  being redistributed implicitly. This enforces conservation by construction
  and avoids implying a unique or fully observable ground truth.

These contributions collectively position Tycho as an accuracy-first research
system for container-level energy attribution under concurrent, heterogeneous,
and short-lived workloads.


---

## Quick links to artifacts

- **Container images (GHCR):**
  ```bash
  # Development tag
  docker pull ghcr.io/casparwackerle/tycho-energy:devel

  # Latest tag
  docker pull ghcr.io/casparwackerle/tycho-energy:latest
  ```
---

## Security note (please read)

Tycho interacts with the kernel, cgroups, and hardware counters. As such:

- Privileged access and root are typically required for local debugging and some data sources (e.g., eBPF attach).
- **Current project focus is research/PoC**, not hardening. The author **does not accept responsibility** for security incidents arising from running this code or its tooling.
- Use Tycho only in environments you control and understand (e.g., lab/test nodes).  
  For cluster deployments, follow standard Kubernetes security best practices and restrict access accordingly.
---

## Documentation overview

Tycho is a research system developed and documented primarily through an
academic thesis. As such, the **authoritative and complete documentation of
Tycho’s conceptual foundations, architectural design, modelling assumptions,
and evaluation methodology is the Master’s thesis itself**:

https://github.com/casparwackerle/PowerStack/tree/main/thesis/MT

The thesis provides the only fully consistent and reviewed description of
Tycho’s design goals, invariants, temporal model, energy attribution logic,
and experimental validation. Readers seeking a deep or correct understanding
of Tycho should start there.

---

## Related Projects

### Upstream Kepler

This project is a research fork based on **Kepler v0.9**.  
For the upstream project, see: https://github.com/sustainable-computing-io/kepler

Note: The Kepler project received a major re-write with **v0.10**.  
Due to the nature of these changes, **v0.10+ is not backward-compatible with this fork** (which tracks the v0.9 design).  

Tycho’s author has the utmost respect for the Kepler maintainers and contributors. The upstream project continues to evolve rapidly. Please refer to it for the latest features and roadmap.

### PowerStack

**PowerStack** is a Kubernetes-based infrastructure automation project designed fully automated bare-metal cluster setup.  
Repository: https://github.com/casparwackerle/PowerStack

Tycho and PowerStack are complementary but clearly separated: Tycho is the energy
attribution system itself, while PowerStack is used to provision the testbed
(K3s + Rancher + storage), deploy Kepler/Tycho, run benchmarks, and collect and
visualize energy data.


#### Integration with Tycho

- PowerStack provides a reproducible environment for **building, deploying, and validating** Tycho.
- It also integrates with GHCR and Helm/DaemonSets to mirror Tycho’s **cluster deployment** path used in research.

#### Related scientific works (in the PowerStack repo)

PowerStack’s repository also hosts the author’s scientific work that underpins this project:

1. **VT1 – PowerStack (Development & Evaluation)**  
   https://github.com/casparwackerle/PowerStack/tree/main/thesis/VT1  
   **Abstract (short):**  
   > Investigates energy consumption at the container and node level in Kubernetes-based infrastructures using Kepler.  
   > A bare-metal, automated K3s cluster (Ansible) was built to collect Prometheus/Grafana metrics under controlled CPU/memory/disk/network workloads.  
   > Findings: Kepler credibly tracks workload-induced power at the CPU package level; non-CPU domains show inconsistencies; high idle node power highlights static consumption.  
   > Provides a foundation for further work on measurement accuracy, workload profiling, and automation-driven optimization.

2. **VT2 – _Container-Level Energy Consumption Estimation: Foundations, Challenges, and Current Approaches_**  
   https://github.com/casparwackerle/PowerStack/tree/main/thesis/VT2  
   **Abstract (short):**  
   > A survey-style thesis on the theory, challenges, and current approaches to container-level energy attribution in bare-metal Kubernetes.  
   > Analyzes measurement techniques, attribution complexity (shared resources, limited telemetry), and tooling limits.  
   > Concludes with methodological gaps, validation challenges, and recommendations to advance energy transparency.

3. **MT – Master’s Thesis (Tycho)**  
   https://github.com/casparwackerle/PowerStack/tree/main/thesis/MT  
   **Abstract:** 
   > Accurately attributing energy consumption to individual workloads in containerized environments is challenging due to shared hardware resources, limited observability, and asynchronous, heterogeneous telemetry. This thesis presents \emph{Tycho}, a novel, accuracy-first system for container-level power attribution that departs from window-delta-based approaches by retaining high-frequency measurement data in bounded historical buffers and deferring temporal reconciliation to analysis time. Independent metric collectors operate at source-appropriate frequencies, preserving native temporal structure and enabling post hoc alignment and correlation across diverse energy and utilization signals.
   > Building on an extensive review of existing power measurement and attribution approaches, the thesis develops a principled framework that explicitly models observation delay, enforces energy conservation by construction, and treats idle and residual energy as first-class outcomes. Tycho integrates CPU, GPU, and system-level energy signals, including composite GPU energy modelling from heterogeneous telemetry and delay-aware refinement of coarse system energy measurements using fine-grained subsystem proxies. The system is evaluated qualitatively and quantitatively on representative and concurrent workloads, demonstrating accurate and temporally coherent attribution behaviour across diverse execution scenarios without implying a unique ground truth.
   > Tycho is released as an open-source contribution to support reproducibility and further research in energy-aware systems. Experimental deployment and evaluation are supported by PowerStack, an auxiliary framework for fully automated installation and test environment setup.

---

## License

With the exception of eBPF code, everything is distributed under the terms of the [Apache License (version 2.0)].

### eBPF

All eBPF code is distributed under either:

- The terms of the [GNU General Public License, Version 2] or the [BSD 2 Clause license], at your option.
- The terms of the [GNU General Public License, Version 2].

The exact license text varies by file. Please see the SPDX-License-Identifier header in each file for details.

Files that originate from the authors of kepler use (GPL-2.0-only OR BSD-2-Clause). Files generated from the Linux kernel i.e vmlinux.h use GPL-2.0-only.

Unless you explicitly state otherwise, any contribution intentionally submitted for inclusion in this project by you, as defined in the GPL-2 license, shall be dual licensed as above, without any additional terms or conditions.

[apache license (version 2.0)]: LICENSE-APACHE
[apache2-badge]: https://img.shields.io/badge/License-Apache%202.0-blue.svg
[apache2-url]: https://opensource.org/licenses/Apache-2.0
[bsd 2 clause license]: LICENSE-BSD-2
[bsd2-badge]: https://img.shields.io/badge/License-BSD%202--Clause-orange.svg
[bsd2-url]: https://opensource.org/licenses/BSD-2-Clause
[gnu general public license, version 2]: LICENSE-GPL-2
[gpl-badge]: https://img.shields.io/badge/License-GPL%20v2-blue.svg
[gpl-url]: https://opensource.org/licenses/GPL-2.0

---

## Author & contact

- **Author:** Caspar Wackerle (Master’s thesis project).
- **Contact:** Please use the **LinkedIn link on the author’s GitHub profile** for professional contact details.

For Tycho-specific issues, file GitHub issues in this repository.  
For upstream Kepler questions/bugs, prefer the upstream project.
