# pkg/ — Upstream Kepler framework code

The [`pkg/`](./) directory contains framework and support code originating from the upstream **Kepler** project.

Tycho is implemented primarily under [`internal/tycho/`](../internal/tycho/). The code in `pkg/` is largely retained from upstream and remains in the repository to avoid risky or low-value refactoring shortly before thesis submission. Only sporadic parts of this code are still used, mainly to preserve compatibility with existing collector and exporter integration paths.

For understanding Tycho’s architecture, implementation, and contributions, readers should focus on:

- [`internal/tycho/`](../internal/tycho/) — Tycho implementation
- [`doc_tycho/`](../doc_tycho/) — project-specific documentation and design rationale

The presence of upstream code in `pkg/` is intentional. This repository is meant to serve as a stable, reproducible research artifact rather than a minimal or fully refactored standalone library.

Some low-level components (for example, eBPF-related parts) were adapted directly within their upstream locations where this represented the least disruptive integration strategy.
