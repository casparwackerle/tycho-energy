# Tycho documentation (`doc_tycho/`)

This directory contains **auxiliary, development-facing documentation** for the
Tycho project.

⚠️ **Authoritative documentation notice**

Tycho is a research system developed and evaluated as part of an academic
Master’s thesis. The **authoritative and complete documentation of Tycho’s
conceptual foundations, architectural design, modelling assumptions, temporal
semantics, and evaluation methodology is the thesis itself**, not the Markdown
files in this repository.

The thesis is hosted in the PowerStack repository and should be considered the
primary reference for understanding Tycho:

https://github.com/casparwackerle/PowerStack/tree/main/thesis/MT

Readers seeking a correct, rigorous, and complete description of Tycho’s design
and behavior should start with the thesis.

---

## Purpose of this directory

The files in `doc_tycho/` serve a deliberately limited role:

- to support **development, debugging, and experimentation**
- to document **practical setup and workflow details**
- to provide orientation within the codebase where appropriate

This directory does **not** attempt to restate, summarize, or replace the thesis.
Conceptual correctness, invariants, and design rationale are defined exclusively
by the academic document.

---

## Contents

- **`DEVELOPMENT.md`**  
  Describes the local development environment, build workflow, debugging setup,
  and practical notes for working with Tycho’s codebase.

No additional feature-level or architectural documentation is maintained here at
this time. This is intentional and reflects Tycho’s status as a research artifact
rather than a production software project.

---

## Relationship to PowerStack

PowerStack is a separate repository that provides infrastructure automation for
provisioning testbeds, deploying Tycho, running experiments, and collecting
evaluation data:

https://github.com/casparwackerle/PowerStack

All thesis documents (VT1, VT2, and the Master’s thesis) are maintained there.
This repository focuses exclusively on the Tycho system itself.

---

## Documentation philosophy

- The **thesis defines correctness**.
- Repository documentation supports **practical work only**.
- Duplication of conceptual material is avoided by design.
- Absence of feature-level Markdown documentation should not be interpreted as
  missing or incomplete specification.

This separation is intentional and appropriate for a research-focused system.
