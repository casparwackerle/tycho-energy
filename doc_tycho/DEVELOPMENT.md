# DEVELOPMENT.md

This document describes how Tycho is developed and debugged. It explains why development happens on a remote, enterprise-grade host, how the debug loop works, and what software the host needs. It also outlines the configuration layout and the wrapper scripts that start a headless Delve session you can attach to from VS Code. After reading this, you should understand the rationale for the environment and be able to *reproduce the development/debug setup.

---

## 1) Why develop on a remote enterprise node?

While you *can* debug inside a local KIND cluster, Tycho is research-focused and benefits from hardware that resembles production servers:

- **Telemetry availability & fidelity:** Enterprise nodes expose **Redfish** platform power, richer/cleaner **RAPL**, and vendor accelerators (e.g., **NVML**) more consistently than laptops/desktops.
- **Accuracy over overhead:** Tycho pursues higher-fidelity signals (short intervals, decoupled sources, new estimators). Running on the same node that runs Kubernetes lets you observe real cgroups, PMU/eBPF behavior, and platform power concurrently.
- **Simpler mental model:** Production still deploys as a DaemonSet, but for development you run the binary locally under Delve, step through code and validate metrics immediately.

---

## 2) How the development loop works (high-level)

There are two complementary flows:

1) **Local binary under Delve (primary dev/debug):**
   - Build the Tycho binary locally.
   - Use a config directory containing `config.yaml` (toggles) and a separate Redfish CSV (secrets).
   - Start **headless Delve** (as root) with `--config-dir` pointing to that directory.
   - **Attach VS Code** to the running Delve server and set breakpoints. `/metrics` is served locally for validation.

2) **Container > GHCR > DaemonSet (integration):**
   - Build/push an image to **GHCR** (e.g., `ghcr.io/<user>/tycho-energy:devel`).
   - Deploy via Helm/PowerStack as a privileged DaemonSet for cluster-level checks.
   - This flow is especially useful to validate behavior across nodes.

> A short pointer to PowerStack (cluster bootstrap, automation, and experiments):  
> https://github.com/casparwackerle/PowerStack

---

## 3) Host assumptions & prerequisites (no install steps here)

**Host & OS**
- The **same host** also runs Kubernetes.
- Linux kernel with eBPF/perf support and necessary privileges (Tycho debugged on **5.15**; similar modern kernels should work).
- Root access for eBPF/PMU and Redfish as needed.

**Core software**
- **Go** (recent; e.g., Go **1.21+**).
- **Delve** (DAP-capable; e.g., **v1.9+**).
- **Make** (used by the build).
- **LLVM/Clang** toolchain (for BPF generation; provides `llvm-strip`).
- **yq v4+** (Mike Farah) to read YAML in the wrapper script.
- **cpuid** (binary inspection used by Tycho).
- **Container runtime**: Docker or Podman (for image builds).
- **Kubernetes**: K3s (or equivalent), plus kubectl.
- **VS Code** on your local machine, with:
  - **Remote-SSH** extension.
  - **Go** extension **installed on the remote host** (common gotcha).

---

## 4) Configuration & credentials (how Tycho reads settings)

Create a configuration directory and file (example): tycho-energy/kepler.config/config.yaml
`config.yaml` is your single source of truth for toggles (e.g., `ENABLE_EBPF_CGROUPID`, `EXPOSE_HW_COUNTER_METRICS`, `KEPLER_LOG_LEVEL`, …).
- The file can follow a Helm-like layout; the scripts read keys from:
  - `extraEnvVars: { KEY: "value" }`  
  - or top-level keys (if you prefer)
- Redfish credentials live outside the repo, e.g.: $HOME/Documents/git/redfish.csv
The scripts pass this path to the binary at runtime. Keeping it separate avoids committing secrets.

**Where paths are specified**
- The **config directory** is passed to the binary with `--config-dir …`.
- The **Redfish CSV** path is passed via `--redfish-cred-file-path …` (in our wrapper).
- The **scripts** print the resolved paths on start, for quick verification.

---

## 5) Building locally (what happens)

From the repo root:
- `make _build_local` builds the exporter at `output/bin/<goos><goarch>/kepler`
- The build invokes `go generate` over BPF packages; the **LLVM toolchain** must be present (`llvm-strip` etc.).  
- If BPF tests/tooling are used, some distros require `bcc`/kernel headers. For normal exporter development, Go + LLVM/Clang are typically sufficient.

Conceptually:
- Tycho combines **kernel/eBPF + userspace collectors** and merges them with **platform power sources** (ACPI/Redfish/etc.).
- The debug binary is the same code as the container image; only the _packaging_ differs.

---

## 6) Debugging setup (what you actually run)

We use two scripts placed under `scripts/`:

### `scripts/run-dlv.sh` — start Delve + Tycho
- Purpose: Launch **headless Delve** (as root) and exec the Tycho binary with a few stable flags.
- Binds Delve to `127.0.0.1:40000` and uses `--only-same-user=false` to allow attaching from your SSH user.
- Accepts environment overrides like:
- `CONFIG_DIR` (defaults to your `kepler.config` dir),
- `KEPLER_BIN` (path to the built binary),
- `REDFISH_CSV`,
- `VERBOSE` (maps to `--v=`),
- low-level toggles like `EXPOSE_HW_COUNTER_METRICS`, `ENABLE_EBPF_CGROUPID`, etc.
- Prints a summary (resolved paths, verbosity) before starting.

### `scripts/run-from-config.sh` — read config.yaml → env → start
- Purpose: Parse `config.yaml` and export the relevant **env toggles** (e.g., `ENABLE_GPU`, `EXPOSE_*`, `KEPLER_LOG_LEVEL`), then call `run-dlv.sh`.
- Supports multiple YAML shapes, notably `extraEnvVars:` (common in Helm values files).
- Shows `[cfg] KEY=value` lines for what it picked up, so you can confirm intent.
- Keeps **flags minimal**; config stays the single source of truth.

**Start your debug session**
```bash
# from repo root
./scripts/run-from-config.sh
# (optionally point to a different config file)
./scripts/run-from-config.sh /path/to/another/config.yaml
```
You should see:
- Delve listening on `127.0.0.1:40000`
- Tycho starting with your `--config-dir` and (optionally) `--redfish-cred-file-path`
- Logs honoring `KEPLER_LOG_LEVEL`

## 7) Attaching from VS Code

On the remote host, create `.cscode/launch.json`:
```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Attach to Delve (Tycho)",
      "type": "go",
      "request": "attach",
      "mode": "dap",
      "host": "127.0.0.1",
      "port": 40000
    }
  ]
}
```
Steps:
1. Start `./scripts/run-from-config.sh` in a terminal (keep it running).
2. In VS Code (connected via Remote-SSH), select Run and Debug > Attach to Delve (Tycho).
3. Set breakpoints, watch variables, goroutines, etc.
4. Validate outputs at `http://<host>:8888/metrics`.

## 8) Optional: Image Build and cluster deploy (integration)

When you need to need to validate the containerized path:
- Build and tag to GHCR (e.g. `gicr.io/<user>tycho-energy:devel`).
- Deploy as a **privileged DeamonSet** via Helm/[PowerStack](https://github.com/casparwackerle/PowerStack).
This mirrors production behavior while your day-to-day debugging still uses the **local Delve** approach.

## 9) Summary
- We develop on a remote enterprise node to access realistic telemetry (Redfish, RAPL, NVML) and accurate cgroup contexts.
- The local-under-Delve method enables breakpoints, inspection, and quick iteration while serving Prometheus metrics.
- A config directory (`kepler.config/`) keeps runtime switches declarative; Redfish credentials remain separate.
- Two small scripts (`run-from-config.sh` > `run-dlv.sh`) make the workflow repeatable and easy to share.