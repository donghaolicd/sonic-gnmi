# Solution Design: Low-Footprint Local Developer Sandbox for sonic-gnmi (ADO-YAML-as-Source-of-Truth)

> **Status:** Draft (rev 2) · **Revision notes:** Corrected `$(...)` resolver semantics to ADO-faithful known-name-only substitution (preserves shell command substitution and nesting); added runtime `condition:` handling; documented bracket-index/predefined-variable grammar; reconciled file-count footprint; corrected `pure_tests` grounding (mgmt-common checkout, `go mod tidy` network step, `submodules: recursive`).
> **Location of tool:** `dev/` (dev-only, kept out of the production pipeline path)
> **Authoritative inputs (unmodified):** `azure-pipelines.yml`, `.azure/templates/*.yml`

---

## Executive Summary

We propose a small, dev-only sandbox under `dev/` that lets a developer reproduce the CI steps defined in `azure-pipelines.yml` and `.azure/templates/*.yml` on their own machine inside the same `sonic-slave-trixie` Docker image that CI uses — **without modifying the production ADO YAML and without copying pipeline logic into a parallel set of committed shell scripts**. The core insight from the research is that **no mature open-source "nektos/act for Azure DevOps" exists** (the closest tools require a live ADO server because ADO does template/`${{ }}` expansion server-side), and that **the SONiC org already provides the heavy machinery we need** (the `sonic-slave-trixie` container and the verbatim `dpkg-buildpackage` step bodies). Therefore the recommended approach is a **thin Python "reader/executor"** that *parses the ADO YAML as data at runtime*, resolves the subset of ADO semantics our pipeline actually uses (template includes, `parameters`, simple `${{ if }}` conditional insertion, `$(VAR)` substitution, and `- script:`/`- bash:` step bodies), **stubs the three `DownloadPipelineArtifact@2` tasks that have no local equivalent**, and shells the resolved step bodies into the slave container. The ADO YAML stays canonical: the sandbox reads it, never rewrites it. Net committed footprint target: **one Python package + a thin entrypoint + a stub-config file + README + tests under `dev/`** (9 files; see *Files Affected*), zero changes to production pipeline files.

---

## Background

### Current CI architecture (grounded in the repo)

`azure-pipelines.yml` defines three stages:

| Stage | Job(s) | Environment | What it runs |
|-------|--------|-------------|--------------|
| `StaticChecks` | `go_static_checks` | bare `ubuntu-22.04` | `install-go.yml` + inline `gofmt` bash step |
| `Test` | `pure_tests` | bare `ubuntu-22.04` | `install-go.yml` + `make -f pure.mk junit-xml` |
| `Test` | `memleak_tests`, `integration_tests` | container `sonicdev-microsoft.azurecr.io:443/sonic-slave-trixie:latest` | `setup-test-env.yml` + `make all` + `make check_memleak_junit` / `make check_gotest_junit` |
| `Test` | `build` | bare `ubuntu-22.04` | coverage aggregation (PR-only, diff-cover) |
| `Package` | `amd64`, `arm64` | slave container | `build-deb.yml` |

The four templates under `.azure/templates/` encapsulate the reusable logic:

- **`install-go.yml`** — downloads Go `$(GO_VERSION)` to `/usr/local/go`. Pure `- script:`.
- **`install-dependencies.yml`** — the dependency core. Mixes `- script:` steps (apt/dpkg/pip) with **three `DownloadPipelineArtifact@2` tasks** and **`${{ if }}` conditional insertion** keyed on `parameters.arch` and `parameters.installTestDeps`.
- **`setup-test-env.yml`** — multi-repo `- checkout:` (self + `sonic-mgmt-common` + `sonic-swss-common`), then `install-dependencies.yml`, then `dpkg-buildpackage` of mgmt-common.
- **`build-deb.yml`** — checkout + deps + build mgmt-common + build sonic-gnmi `.deb` + publish.

The pipeline already has a **mostly-local, low-dependency tier**: the `pure_tests` job runs `make -f pure.mk junit-xml` over 15 pure Go packages. Note this job is not fully offline as written — it also does `- checkout: sonic-mgmt-common` and a `go mod tidy` (which may hit the network) before `make -f pure.mk junit-xml` — but it needs **none** of the three ADO-only artifacts or the slave container, making it the cheapest tier to reproduce.

### What changed / why now

A prior "single source of truth" attempt **extracted each inline ADO step into committed scripts** under `scripts/` (11 files, ~2600 lines incl. test harnesses) and rewired the pipeline to call them. This **increased** repo churn and CI risk (the production YAML became a thin caller of bespoke scripts that themselves needed testing) and was abandoned. The header comments in `azure-pipelines.yml` (e.g. the deliberate `job: build`/`displayName: "build"` naming tied to the `coverage.sonic-net.sonic-gnmi.build` GitHub check) show the production pipeline is intentionally fragile in places and must not be casually refactored.

### Prior art in the org (reuse targets)

- **`sonic-buildimage`** ships a docker-based local build system (`Makefile.work`'s `DOCKER_RUN` idiom, `sonic-slave-bash` target) and the **`sonic-slave-trixie` image source**. The image used by CI lives in a **private ACR (`sonicdev-microsoft.azurecr.io:443`) and is not publicly pullable**; it can be built from source via `make BLDENV=trixie sonic-slave-build`.
- **`pure.mk`** in this repo is the lowest-dependency gate (`make -f pure.mk junit-xml`, 15 pure Go packages, no SONiC artifacts/container); the `pure_tests` job wrapping it does add a `checkout: sonic-mgmt-common` and `go mod tidy` that may touch the network.
- The three `DownloadPipelineArtifact@2` sources (`common-lib`, `sonic-buildimage.vs` wheels, `sonic-swss-common-trixie`) are **ADO-only with no public download URL** — this is the central "last mile" gap.

---

## Problem Statement

Developers cannot cheaply reproduce CI failures locally. The only faithful reproduction is the ADO pipeline itself, which:

1. Requires ADO infrastructure and the private slave image / artifacts.
2. Has its step logic spread across one pipeline file + four templates using ADO-specific constructs (`${{ }}`, `$()`, conditional insertion, `DownloadPipelineArtifact@2`).

Two failed/undesirable ways to close the gap:

- **Duplicate the logic into committed shell scripts** (the abandoned approach) → churn, drift, and a second thing to test.
- **Hand-write a local "do the same thing" script** → immediately drifts from the YAML; defeats "single source of truth".

We need the YAML to **remain the one source of truth** while being **executable locally** with the **smallest possible committed footprint**.

---

## Goals and Non-Goals

### Goals
- **G1.** Reproduce locally, in the same `sonic-slave-trixie` container, the behavior of: `install-dependencies`, `setup-test-env`, `build-deb`, and the pure + integration (+ memleak) test steps.
- **G2.** Treat `azure-pipelines.yml` and `.azure/templates/*.yml` as **read-only, canonical** inputs. The sandbox parses them at runtime; it never edits or regenerates them.
- **G3.** **Smallest repo footprint:** one dev-only tool under `dev/`, no new files on the production pipeline path, no new committed shell scripts that duplicate step bodies.
- **G4.** Cleanly **stub/mock** the constructs with no local equivalent (`DownloadPipelineArtifact@2`, `publish`, `PublishTestResults@2`, `PublishCodeCoverageResults@2`) via an explicit, documented config — not by editing YAML.
- **G5.** Provide a **dependency-acquisition path** for the three ADO-only artifacts (documented build-from-source / local-cache strategy).

### Non-Goals
- **NG1.** A general-purpose, spec-complete ADO interpreter. We implement only the subset of ADO semantics our four templates use; anything else fails loudly.
- **NG2.** Reproducing ADO-cloud-only features: artifact publishing to ADO, the diff-cover pipeline decorator, the `coverage.sonic-net.sonic-gnmi.build` GitHub check, multi-agent pools, `arm64` cross-arch (out of scope for v1; amd64 only).
- **NG3.** Changing CI behavior or the production pipeline files in any way.
- **NG4.** Building/publishing the slave image ourselves (we reuse sonic-buildimage's image; we only document how to obtain it).
- **NG5.** Windows support (Linux/Docker host only, matching CI).

---

## Requirements

### Functional
- **FR1.** Load `azure-pipelines.yml` + referenced templates and resolve them into a flat, ordered list of executable shell steps for a chosen job.
- **FR2.** Support the ADO subset actually used:
  - `template:` includes with `parameters:` passing.
  - `parameters:` with `type`/`default`/`values`.
  - `${{ if <cond> }}:` / `${{ else }}:` **conditional insertion** over `parameters.*` and `variables.*` (dotted access) and **bracket-index access** `variables['Name']` (used by the `variables:` block's `${{ if eq(variables['Build.Reason'], 'PullRequest') }}`) using `eq()`/`ne()`/`and()`/`or()`/`not()`.
  - `${{ parameters.x }}` and `${{ variables.x }}` **compile-time substitution** into strings.
  - `$(VAR)` **runtime macro** substitution, applied **ADO-faithfully**: replace `$(Name)` **only** when `Name` matches a *known* variable (from `variables:`, job `variables:`, the pseudo-var table, or overrides); **leave every other `$(...)` byte-for-byte verbatim**, including shell command substitutions and nested forms (see *Design Decisions / DD7*).
  - Step kinds: `- script:`, `- bash:` (executed); `- checkout:` (mapped to local sibling dirs; `submodules: recursive`/`fetchDepth`/`clean` accepted and either honored against the local copy or ignored with a logged note — never fail-fast); `- task: <Known>@N` (dispatched to a handler/stub).
  - Step-level `condition:` and job-level `condition:` (see FR7).
- **FR7.** Accept ADO `condition:` fields on steps and jobs. v1 evaluates the trivial/static cases relevant to local runs and treats the rest as a documented default rather than fail-fast:
  - `condition: always()` → always run (matters for the publish/result stubs).
  - `condition: succeeded()` (the default) → run unless a prior step failed.
  - `condition: failed()` → run only after a failure.
  - The `build` job's `condition: and(succeeded(), eq(variables['Build.Reason'], 'PullRequest'))` and any other non-trivial expression → evaluated against the pseudo-var table when all operands are known (`Build.Reason` defaults to `PullRequest` in `sandbox.yaml`), otherwise treated as **true** with a logged note. `condition:` is explicitly **excluded** from the NFR3 fail-fast rule.
- **FR3.** Map `- checkout: self|sonic-mgmt-common|sonic-swss-common` to **existing local sibling working copies** (no network clone by default), reproducing CI's side-by-side layout.
- **FR4.** Stub the no-local-equivalent tasks:
  - `DownloadPipelineArtifact@2` → copy from a developer-provided local artifact cache (path configured in `dev/` config), or no-op with a clear warning if `--allow-missing-artifacts`.
  - `publish` / `PublishTestResults@2` / `PublishCodeCoverageResults@2` → collect outputs into a local results dir; never call ADO.
- **FR5.** Execute resolved `- script:`/`- bash:` bodies **inside the slave container** with the correct `workingDirectory`, env, and `set -e` semantics ADO applies.
- **FR6.** A single entrypoint: `dev/ado-sandbox <job>` (e.g. `pure_tests`, `integration_tests`, `amd64`) with `--list`, `--dry-run` (print resolved steps), and `--shell` (drop into the container).

### Non-Functional
- **NFR1.** Footprint: 9 committed files under `dev/` (see *Files Affected*); **zero** modified/added files outside `dev/`.
- **NFR2.** No new third-party runtime services. Python 3 + PyYAML + Docker CLI only.
- **NFR3.** Fail-fast on any unsupported **structural** ADO construct (unknown `- task:`, unknown step kind, unparseable `${{ }}` expression) with a precise message (file + line + construct), so the tool never *silently* diverges from the YAML. This rule **does not** apply to unresolved `$(...)` macros (preserved verbatim, DD7) nor to `condition:` fields (FR7) — both are normal, expected ADO behavior, not unsupported constructs.
- **NFR4.** `--dry-run` must be runnable with no Docker and no artifacts (pure resolution), for fast iteration and unit testing.

---

## Proposed Design

### Architecture Overview

```
                 dev/ado-sandbox (entrypoint, Python)
                          │
        ┌─────────────────┼──────────────────────────────┐
        ▼                 ▼                                ▼
  ┌───────────┐    ┌──────────────┐                ┌──────────────┐
  │  Loader   │    │   Resolver   │                │   Executor   │
  │ (PyYAML)  │──▶ │ templates +  │──▶ flat steps ─▶│ docker run   │
  │ read-only │    │ ${{ }} + $() │   (list[Step]) │ slave-trixie │
  └───────────┘    │ + conditions │                └──────┬───────┘
        ▲          └──────┬───────┘                       │
        │                 │                               ▼
  azure-pipelines.yml     ▼                        ┌──────────────┐
  .azure/templates/*.yml  Task dispatch            │ Task handlers│
   (CANONICAL, UNTOUCHED) (script/bash/checkout/   │ + STUBS      │
                           task)                   │ (artifacts,  │
                                                   │  publish)    │
                                                   └──────────────┘
                                  config: dev/sandbox.yaml (stubs, caches, var overrides)
```

Key property: **data flows one way out of the YAML.** The YAML is parsed into an in-memory model; nothing is written back. All local-only behavior (stubs, var overrides, artifact cache paths) lives in `dev/sandbox.yaml`, a **sandbox-owned** config — not in the pipeline files.

### Key Components

1. **Loader** (`dev/ado_sandbox/loader.py`)
   - Reads `azure-pipelines.yml` and templates via PyYAML.
   - **Problem:** ADO `${{ if }}` keys are *not valid* as plain YAML mapping semantics we want to evaluate — but they *are* syntactically valid YAML (string keys). We load with a custom constructor that **preserves key order and line numbers** (for error messages) and keeps `${{ ... }}` keys/values as raw strings for the Resolver.
   - Responsibility: produce a raw AST (nested dict/list) + source map. No semantics.

2. **Resolver** (`dev/ado_sandbox/resolver.py`) — the heart.
   - **Template expansion:** when a step is `{'template': path, 'parameters': {...}}`, load that template, bind its `parameters` (defaults overlaid with passed values), and recurse. Templates resolve relative to the including file (matches ADO).
   - **`${{ }}` compile-time layer:** evaluate conditional-insertion keys (`${{ if and(eq(parameters.arch,'amd64'), eq(parameters.installTestDeps, true)) }}:`) and `${{ else }}:` by parsing a **restricted expression grammar** — literals (string/bool), dotted access `parameters.*`/`variables.*`, **bracket-index access** `variables['Name']` (incl. predefined `Build.Reason`), and functions `eq`, `ne`, `and`, `or`, `not`. Substitute `${{ parameters.x }}` / `${{ variables.x }}` occurrences inside strings.
   - **`$(VAR)` runtime layer (ADO-faithful, DD7):** after compile-time resolution, scan for `$(Name)` tokens and substitute **only** those whose `Name` matches a *known* variable, using the precedence chain: CLI `--var` > `dev/sandbox.yaml` overrides > job `variables:` > pipeline `variables:` > a small built-in table for `System.*`/`Build.*`/`Pipeline.*` pseudo-vars (e.g. `Build.ArtifactStagingDirectory`, `System.DefaultWorkingDirectory`, `Pipeline.Workspace`, `Build.Reason`) mapped to sandbox paths/values. **Any `$(...)` whose inner text is not a known variable name is left verbatim** — this is exactly how ADO behaves and is required because real step bodies contain legitimate shell command substitutions (`$(go env GOPATH)`, `-j$(nproc)`) and **nested** forms (`$(find $(Build.ArtifactStagingDirectory)/download -name '*.deb')`, where only the inner ADO macro is replaced and the outer shell `$(find ...)` is preserved). Substitution is innermost-first so nesting resolves correctly; there is **no** fail-fast on "unresolved" `$()`.
   - **`condition:` layer (FR7):** record step/job `condition:` and evaluate `always()/succeeded()/failed()` plus known-operand `eq/and/...` against the pseudo-var table; unknown-operand conditions default to true with a logged note. Never fail-fast on `condition:`.
   - Output: ordered `list[Step]`, each `Step` = `{kind, body|task, displayName, workingDirectory, env, condition}`.

3. **Task Dispatch + Stubs** (`dev/ado_sandbox/tasks.py`)
   - `script`/`bash` → `RunStep` (run body in container).
   - `checkout` → resolve to a local sibling dir (`self` = repo root; named repos = `../<name>` or configured path); no clone by default. `submodules: recursive`, `fetchDepth`, and `clean` modifiers are accepted: `submodules`/`fetchDepth` are best-effort against the existing local copy (logged if the local checkout doesn't satisfy them) and never fail-fast.
   - `task: DownloadPipelineArtifact@2` → **stub**: read `inputs.artifact`/`patterns`, copy matching files from `dev/sandbox.yaml: artifact_cache.<artifact>` into the target `path`. If absent → error with the exact `az pipelines runs artifact download` / build-from-source hint, or warn under `--allow-missing-artifacts`.
   - `task: PublishTestResults@2` / `PublishCodeCoverageResults@2` / `publish:` → **stub**: copy the referenced files into `dev/build-out/results/` and log; never contact ADO.
   - `task: <unknown>@N` → fail-fast.

4. **Executor** (`dev/ado_sandbox/executor.py`)
   - Builds one `docker run` per job (not per step) to preserve in-job state (PATH exports, installed debs), mirroring ADO's "all steps share the job's agent". Mounts the repo + sibling repos + an artifact-cache dir + a results dir, forwards proxy env, applies `--ulimit nofile` and the container image from the YAML's `container.image` (overridable to a locally-built tag via config).
   - Concatenates resolved step bodies into a single bash script with `set -e` and per-step `echo "##[section]<displayName>"` markers, applying each step's `workingDirectory`/`env`. Bare-host jobs (`pure_tests`, `go_static_checks`) optionally run without a container (`--no-container`) since they only need Go.

5. **Config** (`dev/sandbox.yaml`) — the only place local-only knowledge lives:
   ```yaml
   image: sonic-slave-trixie:local        # override the private ACR ref
   repos:
     sonic-mgmt-common: ../sonic-mgmt-common
     sonic-swss-common: ../sonic-swss-common
   artifact_cache:
     common-lib: ~/.cache/sonic-gnmi-sandbox/common-lib
     sonic-buildimage.vs: ~/.cache/sonic-gnmi-sandbox/vs
     sonic-swss-common-trixie: ~/.cache/sonic-gnmi-sandbox/swsscommon
   vars:
     BUILD_BRANCH: master
   ```

### Data Flow (integration_tests job)

1. Loader reads `azure-pipelines.yml`, finds `job: integration_tests` → first step is `template: .azure/templates/setup-test-env.yml` with `buildBranch: $(BUILD_BRANCH)`, `fetchDepth: 0`.
2. Resolver expands `setup-test-env.yml`: emits `checkout self/mgmt-common/swss-common`, then recursively expands `install-dependencies.yml` (`arch=amd64`, `installTestDeps=true`).
3. In `install-dependencies.yml`, the `${{ if and(eq(arch,'amd64'), eq(installTestDeps,true)) }}` block is **kept**; the arm64 blocks are **dropped**. `DownloadPipelineArtifact@2` steps become stub tasks bound to `artifact_cache`.
4. `$(Build.ArtifactStagingDirectory)`, `$(Pipeline.Workspace)` resolve to sandbox mount paths; `$(BUILD_BRANCH)` → `master`.
5. Executor: `checkout` steps verified against local sibling dirs → mounts them → runs the concatenated `apt/dpkg/pip` + `dpkg-buildpackage` + `make all` + `make check_gotest_junit` bodies in `sonic-slave-trixie:local`.
6. `PublishTestResults@2` stub copies `test-results/junit-integration-*.xml` to `dev/build-out/results/`.

### API / CLI Contract

```
dev/ado-sandbox <job> [options]
  --list                 List jobs/stages discovered in the YAML.
  --dry-run              Resolve + print the flat step list (no Docker, no artifacts).
  --no-container         Run bare-host (pure_tests, go_static_checks).
  --shell                Resolve env + mounts, drop into an interactive container shell.
  --var NAME=VALUE       Override a runtime variable (highest precedence).
  --allow-missing-artifacts   Turn DownloadPipelineArtifact stubs into warnings.
  --config PATH          Path to sandbox.yaml (default: dev/sandbox.yaml).
```

### Design Decisions

- **DD1. Parse-at-runtime, never regenerate.** The YAML is the single source of truth precisely because the tool reads it live. No committed derived scripts → no drift, minimal footprint. (Directly answers the abandoned-approach failure.)
- **DD2. Implement only the used ADO subset; fail-fast otherwise.** Our four templates use a *small, enumerable* slice of ADO. A spec-complete interpreter is a non-goal (NG1) and the research shows it is a multi-month reverse-engineering effort. Fail-fast guarantees we never silently diverge.
- **DD3. Python, not Go.** Resolution is string/tree manipulation; Python + PyYAML is the lowest-effort, most-readable fit and keeps the tool off the production Go build path. (The repo already uses Python for tests/tools.)
- **DD4. Reuse `sonic-slave-trixie`, don't build a new image.** The org already maintains the exact CI environment; we mount into it. Config indirection (`image:`) lets devs point at a locally-built tag since the ACR image is not public.
- **DD5. Stubs live in config + handlers, not in YAML.** `DownloadPipelineArtifact@2` has no local equivalent; we externalize the local artifact source so the canonical YAML stays untouched.
- **DD6. One container per job.** Preserves in-job mutable state (PATH, installed debs) exactly as ADO's single-agent job model does.
- **DD7. ADO-faithful `$()` macro substitution (known-name-only, preserve-the-rest).** ADO does not treat `$(...)` as "must resolve"; it replaces only tokens matching a known pipeline variable and leaves all other `$(...)` untouched. We mirror this exactly. This is **load-bearing**: the real target steps contain shell command substitutions that are *not* ADO macros — `export PATH=$PATH:/usr/local/go/bin:$(go env GOPATH)/bin` (`pure_tests`), `dpkg-buildpackage ... -j$(nproc)` (`build-deb.yml`), and the nested `sudo dpkg -i $(find $(Build.ArtifactStagingDirectory)/download -name '*.deb')` (`install-dependencies.yml`). A naive "fail-fast on unresolved `$()`" rule would wrongly abort three of the four target jobs. We substitute innermost-first so the nested case resolves the inner `$(Build.ArtifactStagingDirectory)` while preserving the outer `$(find ...)` verbatim for the container shell to execute.

---

## Alternatives Considered

| Option | Pros | Cons | Verdict |
|--------|------|------|---------|
| **A. Adopt an OSS ADO local runner** (`amalabey/azp-local-runner`, `rodmtl/ado-pipelines-local-runner`) | Zero bespoke code if it fit | `azp-local-runner` **requires a live ADO org + PAT** (ADO expands YAML server-side) → not offline, 18★ one-person project; `rodmtl` is a **validator only**, 0★, no execution, no Docker. **No `act`-equivalent exists for ADO.** | **Rejected** as the executor. *May reuse `rodmtl`/the VS Code language server for optional static validation only.* |
| **B. Abandoned: extract steps into committed `scripts/` + rewire pipeline** | "Single source" in scripts | +11 files / +2600 lines, production YAML churn, CI risk, second test surface | **Rejected** (already abandoned; this design exists to replace it). |
| **C. Thin Python reader/executor (recommended)** | Smallest footprint, YAML stays canonical, no CI risk, dev-only | Must implement a small ADO subset + maintain it if templates grow | **Chosen.** |
| **D. Hand-written `sandbox.sh` that "does the same thing"** | Trivial to start | Immediately drifts from YAML; not a source of truth; still committed duplication | **Rejected.** |
| **E. Reuse sonic-buildimage `sonic-slave-bash` only (manual)** | Zero new code | No reproduction of the *pipeline steps*; dev must hand-run each command | **Partially adopted** as the container substrate under option C; insufficient alone. |

**Trade-off summary (effort vs. fidelity):**
- Option A (full custom interpreter, spec-complete): **multi-month**, highest fidelity, unjustified for ~4 templates.
- Option C (subset interpreter): **~1.5–3 weeks**, high-enough fidelity for our pipeline, small surface.
- Option D (static script): **days**, low fidelity, violates source-of-truth.

The hard ADO parts and how C handles them:
- **`${{ }}` template expansion** → restricted compile-time evaluator (literals, `parameters/variables`, `eq/ne/and/or/not`). Covers every expression in our templates.
- **`${{ }}` template expansion** → restricted compile-time evaluator (literals, dotted `parameters/variables`, bracket-index `variables['Build.Reason']`, `eq/ne/and/or/not`). Covers every expression in our templates and the `variables:` block.
- **`$()` runtime vars** → ADO-faithful known-name-only substitution (DD7): replace only known variable names, preserve all other `$()` (shell command substitutions, nested forms) verbatim; pseudo-var table maps `System.*/Build.*/Pipeline.*` to sandbox paths/values.
- **`condition:`** → `always()/succeeded()/failed()` honored; known-operand expressions evaluated; unknown default to true (FR7) — never fail-fast.
- **Conditional insertion** → evaluate `${{ if }}`/`${{ else }}` mapping keys; keep/drop child step lists.
- **Multi-repo checkout** → map `resources.repositories` + `- checkout:` to local sibling dirs (no clone by default).
- **`DownloadPipelineArtifact@2` (no local equivalent)** → **stub** copying from a configured local artifact cache; documented build-from-source path for the three ADO-only artifacts.

---

## Dependencies

### External
- **Docker** (host CLI) + the **`sonic-slave-trixie` image** (built from `sonic-buildimage` `make BLDENV=trixie sonic-slave-build`, or pulled if the dev has ACR auth).
- **Python 3.8+**, **PyYAML**.
- Local working copies of **`sonic-mgmt-common`** and **`sonic-swss-common`** as siblings (CI checks them out side-by-side).

### Internal / org
- `sonic-buildimage` slave-image build (reuse).
- The three ADO artifacts (`common-lib`, `sonic-buildimage.vs` wheels, `sonic-swss-common-trixie`) — obtained via local artifact cache (built from source or downloaded by a dev with ADO access).
- `pure.mk` (already present) for the offline tier.

### Sequencing
1. Slave image available locally → 2. Sibling repos present → 3. Artifact cache populated (only for integration/package jobs) → 4. Sandbox runs.

---

## Impact Analysis

- **Codebase touched:** only new files under `dev/`. **No** production pipeline files, Makefiles, or Go code modified.
- **Backward compatibility:** none affected; CI is unchanged because the YAML is unchanged.
- **CI risk:** effectively zero — the tool is never invoked by the pipeline.
- **Operational:** developers must obtain the slave image + (for integration/package) the artifact cache once; documented in `dev/README.md`.
- **Maintenance coupling:** if someone adds a *new* ADO construct to the templates, the sandbox fails-fast (NFR3) signaling a small resolver update — an explicit, visible coupling rather than silent drift.

---

## Security Considerations

- The sandbox runs developer-controlled YAML step bodies inside a container with the repo mounted — same trust model as running `make` locally. No new network listeners.
- `DownloadPipelineArtifact` stub reads only from a developer-configured local cache; it does **not** embed ADO PATs. If a dev opts into real artifact download, they use their own `az`/ADO credentials outside the tool.
- No secrets are read from or written to the canonical YAML.

---

## Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| ADO subset interpreter diverges from real ADO semantics | Medium | Medium | Fail-fast on unknown constructs; `--dry-run` golden tests asserting resolved steps for each job; keep scope to used subset only |
| Slave image not obtainable (private ACR) | Medium | High | Document `sonic-buildimage` build-from-source; `image:` config override; `--no-container` covers pure/static tiers |
| Three ADO-only artifacts unavailable to a dev | Medium | High | `artifact_cache` config + build-from-source docs; `--allow-missing-artifacts` for partial runs; pure tier needs none |
| Templates evolve, breaking the resolver | Low | Low | Fail-fast surfaces it immediately; resolver unit tests run in `pure.mk`-style CI optionally |
| Scope creep into a full ADO emulator | Medium | Medium | NG1 documented; PR review gate on resolver additions |

---

## Open Questions

- **OQ1.** Should the sandbox optionally **clone** missing sibling repos at the right branch, or strictly require pre-existing local copies? (v1: require; flag for clone later.)
- **OQ2.** Is `arm64` reproduction needed for any developer workflow, or is amd64-only acceptable for v1? (Assumed amd64-only.)
- **OQ3.** Do we want an optional integration with `rodmtl/ado-pipelines-local-runner` or the ADO VS Code language server purely for **schema validation** of the canonical YAML, or is that out of scope? (Assumed out of scope for v1.)
- **OQ4.** Where should the artifact cache default live, and do we want a helper `dev/ado-sandbox fetch-artifacts` that uses `az pipelines runs artifact download` when ADO creds exist?
- **OQ5.** Should `--dry-run` golden outputs be committed under `dev/` as regression fixtures (small) to lock resolver behavior?

---

## Implementation Phases

- **Phase 0 — Spike / resolution core (exit: `--dry-run` prints correct flat steps for `pure_tests` and `go_static_checks`, no Docker).**
- **Phase 1 — Executor + bare-host tiers (exit: `pure_tests` and `go_static_checks` run locally and pass, `--no-container`).**
- **Phase 2 — Container + setup-test-env + artifact stubs (exit: `integration_tests` and `memleak_tests` run in `sonic-slave-trixie` given a populated artifact cache).**
- **Phase 3 — build-deb + docs + golden tests (exit: `amd64` deb build reproduced; `dev/README.md` documents acquisition; resolver golden tests in place).**

---

## Files Affected

### New Files
| File Path | Purpose |
|-----------|---------|
| `dev/ado-sandbox` | Thin executable entrypoint (argparse → package). |
| `dev/ado_sandbox/__init__.py` | Package marker. |
| `dev/ado_sandbox/loader.py` | Read YAML + source map (read-only). |
| `dev/ado_sandbox/resolver.py` | Template expansion, `${{ }}` + `$()`, conditional insertion. |
| `dev/ado_sandbox/tasks.py` | Step/task dispatch + stubs (artifact/publish/checkout). |
| `dev/ado_sandbox/executor.py` | Docker run / bare-host execution. |
| `dev/sandbox.yaml` | Dev-only config: image, repo paths, artifact cache, var overrides. |
| `dev/README.md` | How to obtain image/artifacts and run the sandbox. |
| `dev/tests/test_resolver.py` | Unit/golden tests for resolution (run via `--dry-run`). |

> All under `dev/`. Committed footprint = these 9 files (entrypoint, `__init__`, loader, resolver, tasks, executor, `sandbox.yaml`, README, tests), consistent with NFR1 and the Executive Summary. `dev/build-out/` (results) is git-ignored (already untracked).

### Modified Files
| File Path | Changes |
|-----------|---------|
| _none_ | Production pipeline files and Makefiles are **not** modified. |

> Optional (only if desired): add `dev/build-out/` to `.gitignore` — a one-line change, not required if `dev/` artifacts are already ignored.

### Deleted Files
| File Path | Reason |
|-----------|--------|
| _none_ | The abandoned `scripts/` approach is already not present in this branch. |

---

## Implementation Plan

### Epic 1 — Resolution Core (Loader + Resolver, dry-run)  [DONE]
**Goal:** Turn the canonical YAML into a correct flat step list with no execution.
**Prerequisites:** none.

| Task ID | Type | Description | Files | Status |
|---------|------|-------------|-------|--------|
| E1-T1 | IMPL | YAML loader preserving key order + line numbers; keep `${{ }}` keys raw | `dev/ado_sandbox/loader.py` | DONE |
| E1-T2 | IMPL | Template include + `parameters` binding (defaults overlay) with relative-path resolution | `dev/ado_sandbox/resolver.py` | DONE |
| E1-T3 | IMPL | `${{ }}` compile-time evaluator: literals, dotted `parameters/variables`, bracket-index `variables['Build.Reason']`, `eq/ne/and/or/not`; conditional insertion + `${{ else }}` | `dev/ado_sandbox/resolver.py` | DONE |
| E1-T4 | IMPL | ADO-faithful `$()` substitution (DD7): known-name-only, preserve unknown/nested `$()` verbatim, innermost-first; `System/Build/Pipeline` pseudo-var table; `condition:` recording + `always/succeeded/failed` eval | `dev/ado_sandbox/resolver.py` | DONE |
| E1-T5 | IMPL | `--list` / `--dry-run` CLI | `dev/ado-sandbox`, `dev/ado_sandbox/__init__.py`, `dev/ado-sandbox.decisions.md` (architecture decision record documenting resolver/CLI design choices: DD7 macro fidelity, lenient `condition:`/`$()` handling, read-only loader) | DONE |
| E1-T6 | TEST | Golden tests: resolved steps for `pure_tests`, `go_static_checks`, `integration_tests` (arm64 dropped) | `dev/tests/test_resolver.py` | DONE |

**Acceptance Criteria:**
- [x] `dev/ado-sandbox --list` enumerates all stages/jobs from the YAML.
- [x] `dev/ado-sandbox pure_tests --dry-run` prints the exact resolved steps: `checkout self`, `checkout sonic-mgmt-common`, `install-go` (`$(GO_VERSION)`→`1.24.4`), and the `go mod tidy` + `make -f pure.mk junit-xml` body with `$(go env GOPATH)` **preserved verbatim**.
- [x] `install-dependencies` resolution preserves the nested `$(find $(Build.ArtifactStagingDirectory)/download -name '*.deb')` shell substitution while substituting only the inner `$(Build.ArtifactStagingDirectory)` macro; `build-deb` preserves `-j$(nproc)`.
- [x] `integration_tests --dry-run` keeps amd64 `installTestDeps` block, drops all arm64 blocks, lists 3 artifact stubs.
- [x] `variables['Build.Reason']` bracket-index conditional in the `variables:` block evaluates (`BUILD_BRANCH` resolves) and the `build` job's `condition:` evaluates without fail-fast.
- [x] Any unsupported **structural** construct (unknown `task:`/step kind) raises an error naming file + line; unknown `$()` and `condition:` do **not** raise.
- [x] No write occurs to any `.azure/` or root YAML file (verified by test).

### Epic 2 — Executor + Bare-Host Tiers
**Goal:** Actually run the dependency-free jobs locally.
**Prerequisites:** Epic 1.

| Task ID | Type | Description | Files | Status |
|---------|------|-------------|-------|--------|
| E2-T1 | IMPL | Bash assembler: concat step bodies with `set -e`, per-step `workingDirectory`/`env`/section markers | `dev/ado_sandbox/executor.py` | TO DO |
| E2-T2 | IMPL | `--no-container` bare-host run; `install-go.yml` honored | `dev/ado_sandbox/executor.py` | TO DO |
| E2-T3 | IMPL | `checkout` handler → map to local sibling dirs / repo root | `dev/ado_sandbox/tasks.py` | TO DO |
| E2-T4 | IMPL | `publish`/`PublishTestResults@2`/`PublishCodeCoverageResults@2` stubs → copy to `dev/build-out/results/` | `dev/ado_sandbox/tasks.py` | TO DO |
| E2-T5 | TEST | Run `pure_tests` and `go_static_checks` end-to-end; assert exit codes + result files | `dev/tests/test_resolver.py` | TO DO |

**Acceptance Criteria:**
- [ ] `dev/ado-sandbox pure_tests --no-container` runs `make -f pure.mk junit-xml` and produces `junit-pure.xml` collected into `dev/build-out/results/`.
- [ ] `dev/ado-sandbox go_static_checks --no-container` reproduces the `gofmt` gate (passes clean tree, fails on an unformatted file).
- [ ] Publish/checkout stubs never attempt network/ADO calls.

### Epic 3 — Container + setup-test-env + Artifact Stubs
**Goal:** Reproduce the SONiC-dependent test jobs in the slave container.
**Prerequisites:** Epic 2; slave image + artifact cache available.

| Task ID | Type | Description | Files | Status |
|---------|------|-------------|-------|--------|
| E3-T1 | IMPL | `docker run` orchestration: image from config, mounts (repo + siblings + cache + results), proxy env, `--ulimit`, one container per job | `dev/ado_sandbox/executor.py` | TO DO |
| E3-T2 | IMPL | `DownloadPipelineArtifact@2` stub: copy from `artifact_cache.<artifact>` by `patterns`; `--allow-missing-artifacts` warn mode | `dev/ado_sandbox/tasks.py` | TO DO |
| E3-T3 | IMPL | `dev/sandbox.yaml` schema + loading + `--var`/`--config` overrides | `dev/ado_sandbox/__init__.py`, `dev/sandbox.yaml` | TO DO |
| E3-T4 | IMPL | `--shell` interactive mode | `dev/ado_sandbox/executor.py` | TO DO |
| E3-T5 | TEST | `integration_tests` + `memleak_tests` run green in container with a populated cache (documented manual/integration test) | `dev/tests/test_resolver.py`, `dev/README.md` | TO DO |

**Acceptance Criteria:**
- [ ] `dev/ado-sandbox integration_tests` runs `setup-test-env` + `make all` + `make check_gotest_junit` in `sonic-slave-trixie:local` and collects junit + coverage.
- [ ] Missing artifact → precise error with acquisition hint; `--allow-missing-artifacts` downgrades to warning.
- [ ] `--shell integration_tests` drops the dev into the prepared container.

### Epic 4 — build-deb + Docs + Hardening
**Goal:** Reproduce packaging and document the workflow.
**Prerequisites:** Epic 3.

| Task ID | Type | Description | Files | Status |
|---------|------|-------------|-------|--------|
| E4-T1 | IMPL | `amd64` job via `build-deb.yml`: build mgmt-common + sonic-gnmi `.deb`, collect to `dev/build-out/` | `dev/ado_sandbox/executor.py`, `dev/ado_sandbox/tasks.py` | TO DO |
| E4-T2 | DOC | `dev/README.md`: obtain slave image (sonic-buildimage build-from-source), populate artifact cache, run each job | `dev/README.md` | TO DO |
| E4-T3 | IMPL | Optional `fetch-artifacts` helper (uses `az` if creds present) — gated, optional | `dev/ado_sandbox/tasks.py` | TO DO |
| E4-T4 | TEST | Golden + smoke tests across all four target jobs; assert zero writes to canonical YAML | `dev/tests/test_resolver.py` | TO DO |

**Acceptance Criteria:**
- [ ] `dev/ado-sandbox amd64` produces a `sonic-gnmi*.deb` under `dev/build-out/`.
- [ ] `dev/README.md` lets a new developer reproduce all four target step-groups from scratch.
- [ ] Test suite proves the canonical `.azure/` + root YAML are byte-identical before/after any run.
- [ ] Committed footprint stays within `dev/`; no production files changed.

---

## References

- Repo files (canonical inputs): `azure-pipelines.yml`, `.azure/templates/install-dependencies.yml`, `.azure/templates/setup-test-env.yml`, `.azure/templates/build-deb.yml`, `.azure/templates/install-go.yml`, `pure.mk`.
- nektos/act (GitHub Actions local runner): https://github.com/nektos/act
- gitlab-ci-local: https://github.com/firecow/gitlab-ci-local
- Microsoft azure-pipelines-agent (server-coupled; not an offline runner): https://github.com/microsoft/azure-pipelines-agent
- amalabey/azp-local-runner (requires live ADO org): https://github.com/amalabey/azp-local-runner
- rodmtl/ado-pipelines-local-runner (validator only, no execution): https://github.com/rodmtl/ado-pipelines-local-runner
- microsoft/azure-pipelines-vscode (syntax/schema only): https://github.com/microsoft/azure-pipelines-vscode
- ADO pipeline run/processing model (server-side template expansion): https://learn.microsoft.com/en-us/azure/devops/pipelines/process/runs
- ADO templates / expressions: https://learn.microsoft.com/en-us/azure/devops/pipelines/process/templates and https://learn.microsoft.com/en-us/azure/devops/pipelines/process/expressions
- DownloadPipelineArtifact@2: https://learn.microsoft.com/en-us/azure/devops/pipelines/tasks/reference/download-pipeline-artifact-v2
- sonic-buildimage (slave image + `Makefile.work` DOCKER_RUN, `sonic-slave-bash`): https://github.com/sonic-net/sonic-buildimage
- sonic-mgmt-common: https://github.com/sonic-net/sonic-mgmt-common
- sonic-swss-common: https://github.com/sonic-net/sonic-swss-common
