# ado-sandbox

A **dev-only** tool that reproduces the CI steps from `azure-pipelines.yml` and
`.azure/templates/*.yml` on your machine — inside the same `sonic-slave-trixie`
container CI uses — **without modifying the canonical pipeline YAML**. It reads
the YAML as data at runtime, resolves the ADO subset our pipeline uses (template
includes, `${{ }}` compile-time expressions, `$()` runtime macros, conditional
insertion, `condition:`), stubs the constructs with no local equivalent
(`DownloadPipelineArtifact@2`, `PublishTestResults@2`, …), and runs the resolved
step bodies locally.

> Everything lives under `dev/`; no production pipeline file is ever rewritten.

## Quick start

```bash
# List the jobs discovered in the pipeline.
dev/ado-sandbox --list

# Print the fully-resolved step list for a job (no Docker, no artifacts).
dev/ado-sandbox pure_tests --dry-run

# Run a bare-host job (only needs Go; no container).
dev/ado-sandbox pure_tests   --no-container
dev/ado-sandbox go_static_checks --no-container

# Run a SONiC-dependent job inside the slave container.
dev/ado-sandbox integration_tests
dev/ado-sandbox memleak_tests

# Build the amd64 .deb packages (mgmt-common + sonic-gnmi) in the container.
dev/ado-sandbox amd64

# Drop into an interactive shell in the prepared container.
dev/ado-sandbox --shell integration_tests
```

Collected results (junit / coverage) land in `dev/build-out/results/`; the
`amd64` job's `.deb` packages land in `dev/build-out/sonic-gnmi/`.

## Tiers

| Job                | Tier        | Needs                                     |
|--------------------|-------------|-------------------------------------------|
| `pure_tests`       | bare-host   | Go toolchain                              |
| `go_static_checks` | bare-host   | `gofmt`                                   |
| `integration_tests`| container   | slave image + artifact cache              |
| `memleak_tests`    | container   | slave image + artifact cache              |
| `build`, `amd64`   | container   | slave image (+ cache for `integration`)   |

## Configuration — `dev/sandbox.yaml`

`dev/sandbox.yaml` is the **only** place local-only knowledge lives (container
image, sibling-repo paths, artifact-cache directories, variable overrides):

```yaml
image: sonic-slave-trixie:local
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

- `--config PATH` points at an alternate sandbox.yaml (default `dev/sandbox.yaml`).
- `--var NAME=VALUE` overrides a runtime variable with **highest** precedence
  (above `vars:` in the config).

## Container prerequisites

### 1. The slave image

The CI image `sonicdev-microsoft.azurecr.io:443/sonic-slave-trixie:latest` is
**not publicly pullable**. Build it locally from
[`sonic-buildimage`](https://github.com/sonic-net/sonic-buildimage) and tag it to
match `image:` in `dev/sandbox.yaml`:

```bash
# in a sonic-buildimage checkout
make configure PLATFORM=vs
make target/sonic-slave-trixie.tag      # builds sonic-slave-trixie:<hash>
docker tag sonic-slave-trixie:<hash> sonic-slave-trixie:local
```

### 2. The artifact cache (three ADO-only artifacts)

`integration_tests` / `memleak_tests` consume three artifacts that have no public
download URL. Populate each cache directory once, then point
`dev/sandbox.yaml: artifact_cache.*` at it:

| Artifact                   | Contents                                   |
|----------------------------|--------------------------------------------|
| `common-lib`               | libyang / libnl / libpcre `.deb`s          |
| `sonic-buildimage.vs`      | `sonic_yang_models*.whl` python wheels     |
| `sonic-swss-common-trixie` | `libswsscommon*` / `python3-swsscommon` deb|

If you have ADO access, download them with the Azure CLI:

```bash
az pipelines runs artifact download \
  --artifact-name common-lib \
  --path ~/.cache/sonic-gnmi-sandbox/common-lib \
  --run-id <RUN_ID>
```

Or let the optional, gated `fetch-artifacts` helper download all three at once.
It only shells out to `az` when the CLI is installed **and** an ADO credential
is present (`AZURE_DEVOPS_EXT_PAT` or `SYSTEM_ACCESSTOKEN`); otherwise it prints
manual acquisition instructions and exits without touching the network:

```bash
export AZURE_DEVOPS_EXT_PAT=<your-pat>
dev/ado-sandbox fetch-artifacts --run-id <RUN_ID>
```

Otherwise build them from source (sonic-buildimage / sonic-swss-common) and copy
the resulting files into the cache directory, preserving the artifact's internal
layout (e.g. `target/debs/trixie/...`, `target/python-wheels/trixie/...`).

`DownloadPipelineArtifact@2` is stubbed: matching files are copied out of the
cache into the job's workspace **before** the install scripts run. A missing
cache fails with a precise acquisition hint; pass `--allow-missing-artifacts` to
downgrade it to a warning and run as far as possible.

## Verifying the container tier (manual / integration test)

With the image built and the cache populated, the SONiC-dependent jobs run
green and collect junit + coverage into `dev/build-out/results/`:

```bash
# Reproduces setup-test-env + make all + make check_gotest_junit in the container.
dev/ado-sandbox integration_tests

# Same flow, opt-in via pytest (skipped unless Docker + image + cache are present):
ADO_SANDBOX_E2E_CONTAINER=1 \
  python3 -m pytest dev/tests/test_resolver.py -k container_end_to_end
```

## Building the `amd64` .deb packages

The `amd64` job reproduces `.azure/templates/build-deb.yml` inside the slave
container: it checks out `self` + `sonic-mgmt-common` + `sonic-swss-common`,
installs the build dependencies (`arch=amd64`, `installTestDeps=false`), builds
`sonic-mgmt-common` then `sonic-gnmi` via `dpkg-buildpackage -j$(nproc)`, and
collects the produced `.deb` files:

```bash
# Inspect the resolved step list (checkouts, deps, build, publish) — no Docker.
dev/ado-sandbox amd64 --dry-run

# Build the packages (needs the slave image + populated artifact cache).
dev/ado-sandbox amd64
```

The `publish:` step is stubbed: instead of uploading to ADO it copies the
staged outputs into `dev/build-out/sonic-gnmi/`, so a successful run leaves a
`sonic-gnmi_*.deb` (and the mgmt-common / swss-common debs) under
`dev/build-out/`.

## Reproducing all four target step-groups from scratch

| Step-group                        | Command                              | Tier      |
|-----------------------------------|--------------------------------------|-----------|
| `pure_tests`                      | `dev/ado-sandbox pure_tests --no-container`       | bare-host |
| `go_static_checks`                | `dev/ado-sandbox go_static_checks --no-container` | bare-host |
| `integration_tests` / `memleak_tests` | `dev/ado-sandbox integration_tests`          | container |
| `amd64`                           | `dev/ado-sandbox amd64`              | container |

A new developer reproduces the container tiers by (1) building/tagging
`sonic-slave-trixie:local` from `sonic-buildimage` (see *The slave image*
above), (2) populating the artifact cache — from source or via
`dev/ado-sandbox fetch-artifacts --run-id <RUN_ID>` (see *The artifact cache*
above), then (3) running each command in the table.

## Tests

```bash
python3 -m pytest dev/tests/test_resolver.py
```

Resolution and stub logic run fully offline (no Docker, no artifacts). The real
bare-host and container end-to-end tests are opt-in via the `ADO_SANDBOX_E2E`
and `ADO_SANDBOX_E2E_CONTAINER` environment variables.
