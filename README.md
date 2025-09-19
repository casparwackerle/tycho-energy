# Tycho

Tycho is a **research fork of Kepler v0.9** (Kubernetes Efficient Power Level Exporter).  
It aims to provide an accuracy-first approach to container-level energy consumption monitoring in Kubernetes.

## Project overview

- **Motivation:** Upstream Kepler is evolving toward reduced privileges and lower overhead, de-emphasizing eBPF-based collection to improve deployability at scale. While sensible for broad adoption, this shift risks leaving accuracy on the table, accuracy that is crucial for research-grade analysis. Tycho explores that accuracy frontier by **shortening measurement intervals**, **decoupling metric sources** (kernel/eBPF, platform power, accelerators), and introducing **new estimation models** (e.g., device-specific power characteristics).
- **Context:** This repository underpins a **Master’s thesis** and will change as the work progresses. The thesis deadline is January 31st, 2026.
- **Status:** Active research fork; APIs, flags, and configuration **may change without notice**.
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

## Documentation Overview

_TODO_: This section will be updated before project completion. Meanwhile, see [doc_tycho](doc_tycho) and the [Master's thesis documentation](https://github.com/casparwackerle/PowerStack/tree/main/thesis/MT) for the latest Documentation.
- [DEVELOPMENT.md](doc_tycho/DEVELOPMENT.md): Overview of the Development environment for tycho, including detailed setup instructions.

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

Tycho and PowerStack are tightly linked: PowerStack is used to provision the testbed (K3s + Rancher + storage), deploy Kepler/Tycho, run benchmarks, and collect/visualize energy data. **PowerStack will be kept in step with Tycho’s ongoing changes.**

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
   **Abstract:** _TODO (work in progress)._  
   Documents the Tycho system itself, including design choices, configuration model, and evaluation methodology.

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

- **Author:** Repository owner (Master’s thesis project).  
- **Contact:** Please use the **LinkedIn link on the author’s GitHub profile** for professional contact details.

For Tycho-specific issues, file GitHub issues in this repository.  
For upstream Kepler questions/bugs, prefer the upstream project.
