"""Golden / unit tests for the ado-sandbox resolution core (Epic 1).

Run with: ``python3 -m pytest dev/tests/test_resolver.py`` (or plain
``python3 dev/tests/test_resolver.py`` for a lightweight self-check).

These tests exercise pure resolution only -- no Docker, no artifacts (NFR4).
"""

import hashlib
import io
import os
import shutil
import sys

import pytest

DEV_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO_ROOT = os.path.dirname(DEV_DIR)
sys.path.insert(0, DEV_DIR)

import ado_sandbox  # noqa: E402
from ado_sandbox import executor, loader, resolver, tasks  # noqa: E402

PIPELINE = os.path.join(REPO_ROOT, "azure-pipelines.yml")

CANONICAL_FILES = [
    PIPELINE,
    os.path.join(REPO_ROOT, ".azure/templates/install-go.yml"),
    os.path.join(REPO_ROOT, ".azure/templates/install-dependencies.yml"),
    os.path.join(REPO_ROOT, ".azure/templates/setup-test-env.yml"),
    os.path.join(REPO_ROOT, ".azure/templates/build-deb.yml"),
]


def _resolve(job):
    steps, condition = resolver.resolve_job(PIPELINE, job)
    return steps, condition


def _bodies(steps):
    return "\n".join(step.body for step in steps if step.body is not None)


# --------------------------------------------------------------------------
# --list
# --------------------------------------------------------------------------
def test_list_enumerates_all_jobs():
    pipeline = loader.load_yaml(PIPELINE)
    stages = resolver.list_jobs(pipeline)
    stage_names = [stage["stage"] for stage in stages]
    assert stage_names == ["StaticChecks", "Test", "Package"]

    job_names = [job["job"] for stage in stages for job in stage["jobs"]]
    assert job_names == [
        "go_static_checks",
        "pure_tests",
        "memleak_tests",
        "integration_tests",
        "build",
        "amd64",
        "arm64",
    ]


# --------------------------------------------------------------------------
# pure_tests golden
# --------------------------------------------------------------------------
def test_pure_tests_resolved_steps():
    steps, condition = _resolve("pure_tests")
    assert condition is True

    assert steps[0].kind == "checkout" and steps[0].repo == "self"
    assert steps[1].kind == "checkout" and steps[1].repo == "sonic-mgmt-common"

    install_go = steps[2]
    assert install_go.kind == "script"
    assert install_go.display_name == "Install Go 1.24.4"
    # $(GO_VERSION) -> 1.24.4
    assert "go1.24.4.linux-amd64.tar.gz" in install_go.body

    pure = steps[3]
    assert pure.kind == "bash"
    assert "go mod tidy" in pure.body
    assert "make -f pure.mk junit-xml" in pure.body
    # $(go env GOPATH) is a shell substitution, NOT an ADO macro -> verbatim.
    assert "$(go env GOPATH)" in pure.body


# --------------------------------------------------------------------------
# go_static_checks golden
# --------------------------------------------------------------------------
def test_go_static_checks_resolved_steps():
    steps, _ = _resolve("go_static_checks")
    kinds = [step.kind for step in steps]
    assert kinds == ["checkout", "script", "bash"]
    assert steps[1].display_name == "Install Go 1.24.4"
    assert "gofmt" in _bodies(steps)


# --------------------------------------------------------------------------
# install-dependencies / nested $() preservation
# --------------------------------------------------------------------------
def test_nested_macro_preserved_inner_substituted():
    steps, _ = _resolve("integration_tests")
    body = _bodies(steps)
    # Inner $(Build.ArtifactStagingDirectory) substituted, outer $(find ...) kept.
    assert "sudo dpkg -i $(find /sandbox/staging/download -name '*.deb')" in body


def test_build_deb_preserves_nproc():
    steps, _ = _resolve("amd64")
    body = _bodies(steps)
    assert "-j$(nproc)" in body
    assert "cp ../*.deb /sandbox/staging/" in body


# --------------------------------------------------------------------------
# integration_tests: amd64 kept, arm64 dropped, 3 artifact stubs
# --------------------------------------------------------------------------
def test_integration_tests_arch_filtering():
    steps, _ = _resolve("integration_tests")
    downloads = [s for s in steps if s.task == "DownloadPipelineArtifact@2"]
    assert len(downloads) == 3

    display_names = [s.display_name for s in steps if s.display_name]
    # amd64 installTestDeps block is kept.
    assert "Install test dependencies (pytest, redis)" in display_names
    # amd64-only blocks kept; no arm64 blocks present.
    assert "Install libswsscommon (amd64)" in display_names
    assert "Install libswsscommon (arm64)" not in display_names
    body = _bodies(steps)
    assert "_arm64.deb" not in body


# --------------------------------------------------------------------------
# variables block bracket-index conditional + build job condition
# --------------------------------------------------------------------------
def test_variables_block_bracket_index_conditional():
    pipeline = loader.load_yaml(PIPELINE)
    _, job = resolver.find_job(pipeline, "build")
    known = resolver.build_known_vars(pipeline, job)

    # The ${{ if eq(variables['Build.Reason'], 'PullRequest') }} branch was taken.
    assert known["BUILD_BRANCH"] == "$(System.PullRequest.TargetBranch)"
    # ... and BUILD_BRANCH macro resolves (recursively) to a concrete branch.
    assert resolver.substitute_macros("$(BUILD_BRANCH)", known) == "master"


def test_build_job_condition_evaluates_without_failfast():
    steps, condition = _resolve("build")
    # and(succeeded(), eq(variables['Build.Reason'], 'PullRequest')) -> True
    assert condition is True


def test_build_reason_override_changes_condition():
    pipeline = loader.load_yaml(PIPELINE)
    _, job = resolver.find_job(pipeline, "build")
    known = resolver.build_known_vars(pipeline, job, cli_vars={"Build.Reason": "IndividualCI"})
    assert resolver.eval_condition(job.get("condition"), known) is False


# --------------------------------------------------------------------------
# NFR3 fail-fast on structural constructs; never on $() / condition
# --------------------------------------------------------------------------
def test_unknown_step_kind_raises_with_file_and_line(tmp_path):
    bad = tmp_path / "bad.yml"
    bad.write_text(
        "stages:\n"
        "- stage: S\n"
        "  jobs:\n"
        "  - job: j\n"
        "    steps:\n"
        "    - frobnicate: nope\n"
    )
    with pytest.raises(resolver.ResolverError) as exc:
        resolver.resolve_job(str(bad), "j")
    message = str(exc.value)
    assert "bad.yml" in message
    assert ":6" in message


def test_unknown_task_raises_with_file_and_line(tmp_path):
    bad = tmp_path / "bad.yml"
    bad.write_text(
        "stages:\n"
        "- stage: S\n"
        "  jobs:\n"
        "  - job: j\n"
        "    steps:\n"
        "    - task: TotallyUnknown@9\n"
    )
    with pytest.raises(resolver.ResolverError) as exc:
        resolver.resolve_job(str(bad), "j")
    assert "TotallyUnknown@9" in str(exc.value)
    assert ":6" in str(exc.value)


def test_unknown_macro_does_not_raise():
    # $(go env GOPATH), $(nproc) etc. must never raise.
    assert resolver.substitute_macros("a $(nproc) b $(go env X)", {}) == "a $(nproc) b $(go env X)"


def test_unknown_condition_defaults_true_without_raise():
    assert resolver.eval_condition("eq(variables['Nope'], 'x')", {}) is True
    assert resolver.eval_condition("garbage((", {}) is True


# --------------------------------------------------------------------------
# Read-only guarantee
# --------------------------------------------------------------------------
def test_no_writes_to_canonical_files():
    def _sha(path):
        with open(path, "rb") as handle:
            return hashlib.sha256(handle.read()).hexdigest()

    def digest():
        return {path: _sha(path) for path in CANONICAL_FILES}

    before = digest()
    for job in ["go_static_checks", "pure_tests", "integration_tests", "amd64", "arm64", "build"]:
        resolver.resolve_job(PIPELINE, job)
    after = digest()
    assert before == after


# --------------------------------------------------------------------------
# CLI smoke tests
# --------------------------------------------------------------------------
def test_cli_list(capsys):
    ado_sandbox.main(["--list", "--pipeline", PIPELINE])
    out = capsys.readouterr().out
    assert "job: pure_tests" in out
    assert "job: arm64" in out


def test_cli_dry_run(capsys):
    ado_sandbox.main(["pure_tests", "--dry-run", "--pipeline", PIPELINE])
    out = capsys.readouterr().out
    assert "checkout: self" in out
    assert "go1.24.4.linux-amd64.tar.gz" in out
    assert "$(go env GOPATH)" in out


# ==========================================================================
# Epic 2 -- Executor + bare-host tiers
# ==========================================================================
GO_BIN = "/usr/local/go/bin"
HAVE_GOFMT = os.path.exists(os.path.join(GO_BIN, "gofmt")) or bool(
    shutil.which("gofmt")
)


def _step(kind, **kwargs):
    return resolver.Step(kind=kind, **kwargs)


# --------------------------------------------------------------------------
# E2-T1: bash assembler
# --------------------------------------------------------------------------
def test_assemble_script_has_set_e_and_section_markers():
    steps = [
        _step("script", display_name="Install Go", body="go version"),
        _step("bash", display_name="Run tests", body="make test"),
    ]
    script = executor.assemble_script(steps, "/work")
    assert script.startswith("#!/usr/bin/env bash\nset -e\n")
    assert 'echo "##[section]Install Go"' in script
    assert 'echo "##[section]Run tests"' in script
    assert "go version" in script
    assert "make test" in script


def test_assemble_script_applies_working_directory_and_env():
    steps = [
        _step("bash", display_name="step", body="run",
              working_directory="sub", env={"FOO": "bar"}),
    ]
    script = executor.assemble_script(steps, "/work")
    assert "cd /work" in script
    assert "cd sub" in script
    assert "export FOO=bar" in script


def test_assemble_script_rejects_injected_env_key():
    steps = [_step("bash", display_name="step", body="run",
                   env={"FOO=x; echo injected #": "bar"})]
    with pytest.raises(resolver.ResolverError):
        executor.assemble_script(steps, "/work")


def test_assemble_script_ignores_non_script_kinds():
    steps = [
        _step("checkout", repo="self"),
        _step("bash", display_name="step", body="echo hi"),
        _step("publish", body="/tmp/x", artifact="a"),
    ]
    script = executor.assemble_script(steps, "/work")
    assert "echo hi" in script
    # Only the single bash body produces a section marker.
    assert script.count("##[section]") == 1


# --------------------------------------------------------------------------
# E2-T3: checkout handler (local mapping, no clone, never fails)
# --------------------------------------------------------------------------
def test_checkout_single_self_maps_to_repo_root(tmp_path):
    repo_root = str(tmp_path / "sonic-gnmi")
    os.makedirs(repo_root)
    working_dir, mapping = tasks.plan_checkouts(
        [_step("checkout", repo="self")], repo_root, io.StringIO()
    )
    assert working_dir == repo_root
    assert mapping["self"] == repo_root


def test_checkout_multi_maps_to_sibling_dirs(tmp_path):
    repo_root = str(tmp_path / "sonic-gnmi")
    os.makedirs(repo_root)
    steps = [_step("checkout", repo="self"), _step("checkout", repo="sonic-mgmt-common")]
    working_dir, mapping = tasks.plan_checkouts(steps, repo_root, io.StringIO())
    assert working_dir == str(tmp_path)
    assert mapping["self"] == repo_root
    assert mapping["sonic-mgmt-common"] == str(tmp_path / "sonic-mgmt-common")


def test_checkout_missing_local_copy_does_not_fail(tmp_path):
    repo_root = str(tmp_path / "sonic-gnmi")
    os.makedirs(repo_root)
    log = io.StringIO()
    steps = [_step("checkout", repo="self"), _step("checkout", repo="sonic-mgmt-common")]
    tasks.plan_checkouts(steps, repo_root, log)
    # Missing sibling is only a logged warning, never an exception.
    assert "not found" in log.getvalue()


def test_checkout_none_is_excluded(tmp_path):
    # `checkout: none` is a valid ADO construct: it must not appear in the
    # mapping and must not turn a single real checkout into a multi-repo layout.
    repo_root = str(tmp_path / "sonic-gnmi")
    os.makedirs(repo_root)
    steps = [_step("checkout", repo="self"), _step("checkout", repo="none")]
    working_dir, mapping = tasks.plan_checkouts(steps, repo_root, io.StringIO())
    assert "none" not in mapping
    assert working_dir == repo_root
    assert mapping == {"self": repo_root}


# --------------------------------------------------------------------------
# E2-T4: publish / result-publishing stubs (copy to results, never ADO)
# --------------------------------------------------------------------------
def test_publish_stub_copies_to_results(tmp_path):
    src = tmp_path / "coverage-pure.xml"
    src.write_text("<coverage/>")
    results = tmp_path / "results"
    tasks.handle_stub_step(
        _step("publish", body=str(src), artifact="coverage-pure"),
        str(results), io.StringIO()
    )
    assert (results / "coverage-pure.xml").read_text() == "<coverage/>"


def test_publish_test_results_stub_copies_multiple(tmp_path):
    a = tmp_path / "junit-a.xml"
    b = tmp_path / "junit-b.xml"
    a.write_text("<a/>")
    b.write_text("<b/>")
    results = tmp_path / "results"
    step = _step("task", task="PublishTestResults@2",
                 inputs={"testResultsFiles": "%s\n%s" % (a, b)})
    tasks.handle_stub_step(step, str(results), io.StringIO())
    assert (results / "junit-a.xml").exists()
    assert (results / "junit-b.xml").exists()


def test_coverage_stub_copies_summary(tmp_path):
    src = tmp_path / "coverage.xml"
    src.write_text("<c/>")
    results = tmp_path / "results"
    step = _step("task", task="PublishCodeCoverageResults@2",
                 inputs={"summaryFileLocation": str(src)})
    tasks.handle_stub_step(step, str(results), io.StringIO())
    assert (results / "coverage.xml").exists()


def test_stub_missing_file_never_fails(tmp_path):
    results = tmp_path / "results"
    log = io.StringIO()
    step = _step("task", task="PublishTestResults@2",
                 inputs={"testResultsFiles": str(tmp_path / "nope.xml")})
    tasks.handle_stub_step(step, str(results), log)
    assert "not found" in log.getvalue()


# --------------------------------------------------------------------------
# E2-T1 (FR7): deferred-stub condition execution semantics
# --------------------------------------------------------------------------
def test_always_stub_runs_after_failure():
    step = _step("task", task="PublishTestResults@2", condition="always()")
    assert executor._should_run_stub(step, failed=True, stream=io.StringIO()) is True


def test_succeeded_stub_skipped_after_failure():
    step = _step("publish", body="/tmp/x", condition="succeeded()")
    assert executor._should_run_stub(step, failed=True, stream=io.StringIO()) is False
    assert executor._should_run_stub(step, failed=False, stream=io.StringIO()) is True


def test_failed_stub_runs_only_after_failure():
    step = _step("publish", body="/tmp/x", condition="failed()")
    assert executor._should_run_stub(step, failed=True, stream=io.StringIO()) is True
    assert executor._should_run_stub(step, failed=False, stream=io.StringIO()) is False


# --------------------------------------------------------------------------
# E2-T2/T5: dry-run assembles a runnable script with no Docker / artifacts
# --------------------------------------------------------------------------
def test_no_container_dry_run_emits_script(capsys):
    rc = ado_sandbox.main(["pure_tests", "--no-container", "--dry-run",
                           "--pipeline", PIPELINE])
    assert rc == 0
    out = capsys.readouterr().out
    assert "set -e" in out
    assert "make -f pure.mk junit-xml" in out
    assert "##[section]Install Go 1.24.4" in out


# --------------------------------------------------------------------------
# E2-T5: gofmt gate reproduced end-to-end (offline, system gofmt)
# --------------------------------------------------------------------------
_GOFMT_PIPELINE = """\
stages:
- stage: StaticChecks
  jobs:
  - job: go_static_checks
    steps:
    - checkout: self
    - bash: |
        set -euo pipefail
        export PATH=$PATH:%s
        mapfile -t files < <(find . -type f -name '*.go')
        bad=$(gofmt -l "${files[@]}")
        if [ -n "$bad" ]; then
          echo "::error::gofmt found unformatted Go file(s):"
          printf '  %%s\\n' $bad
          exit 1
        fi
        echo "All files properly formatted."
      displayName: 'gofmt'
""" % GO_BIN

_CLEAN_GO = "package main\n\nfunc main() {}\n"
_DIRTY_GO = "package main\nfunc  main()  {  }\n"


def _write_gofmt_job(tmp_path, go_source):
    pipeline = tmp_path / "azure-pipelines.yml"
    pipeline.write_text(_GOFMT_PIPELINE)
    (tmp_path / "main.go").write_text(go_source)
    return str(pipeline)


@pytest.mark.skipif(not HAVE_GOFMT, reason="gofmt toolchain not available")
def test_go_static_checks_gofmt_gate_passes_clean(tmp_path, capfd):
    pipeline = _write_gofmt_job(tmp_path, _CLEAN_GO)
    rc = executor.run_job(pipeline, "go_static_checks", no_container=True)
    assert rc == 0
    assert "All files properly formatted." in capfd.readouterr().out


@pytest.mark.skipif(not HAVE_GOFMT, reason="gofmt toolchain not available")
def test_go_static_checks_gofmt_gate_fails_unformatted(tmp_path, capfd):
    pipeline = _write_gofmt_job(tmp_path, _DIRTY_GO)
    rc = executor.run_job(pipeline, "go_static_checks", no_container=True)
    assert rc == 1
    assert "gofmt found unformatted" in capfd.readouterr().out


# --------------------------------------------------------------------------
# E2-T5: pure-tier result collection end-to-end (PublishTestResults stub)
# --------------------------------------------------------------------------
_PURE_PIPELINE = """\
stages:
- stage: Test
  jobs:
  - job: pure_tests
    steps:
    - checkout: self
    - bash: |
        set -euo pipefail
        mkdir -p test-results
        echo '<testsuite name="pure"/>' > test-results/junit-pure.xml
      displayName: 'Run pure package tests'
    - task: PublishTestResults@2
      displayName: 'Publish pure package test results'
      condition: always()
      inputs:
        testResultsFormat: 'JUnit'
        testResultsFiles: '$(System.DefaultWorkingDirectory)/test-results/junit-pure.xml'
"""


_PURE_FAILING_PIPELINE = """\
stages:
- stage: Test
  jobs:
  - job: pure_tests
    steps:
    - checkout: self
    - bash: |
        set -euo pipefail
        mkdir -p test-results
        echo '<testsuite name="pure"/>' > test-results/junit-pure.xml
        exit 1
      displayName: 'Run pure package tests'
    - task: PublishTestResults@2
      displayName: 'Publish pure package test results'
      condition: always()
      inputs:
        testResultsFormat: 'JUnit'
        testResultsFiles: '$(System.DefaultWorkingDirectory)/test-results/junit-pure.xml'
"""


def test_pure_tier_collects_junit_into_build_out(tmp_path, capfd):
    pipeline = tmp_path / "azure-pipelines.yml"
    pipeline.write_text(_PURE_PIPELINE)
    rc = executor.run_job(str(pipeline), "pure_tests", no_container=True)
    assert rc == 0
    collected = tmp_path / "dev" / "build-out" / "results" / "junit-pure.xml"
    assert collected.exists()
    assert "PublishTestResults@2: collected" in capfd.readouterr().out


def test_pure_tier_always_publishes_after_script_failure(tmp_path, capfd):
    # condition: always() must still collect results even when the script fails.
    pipeline = tmp_path / "azure-pipelines.yml"
    pipeline.write_text(_PURE_FAILING_PIPELINE)
    rc = executor.run_job(str(pipeline), "pure_tests", no_container=True)
    assert rc == 1
    collected = tmp_path / "dev" / "build-out" / "results" / "junit-pure.xml"
    assert collected.exists()


_PURE_HANG_PIPELINE = """\
stages:
- stage: Test
  jobs:
  - job: pure_tests
    steps:
    - checkout: self
    - bash: |
        mkdir -p test-results
        echo '<testsuite name="pure"/>' > test-results/junit-pure.xml
        sleep 9999
      displayName: 'Run pure package tests'
    - task: PublishTestResults@2
      displayName: 'Publish pure package test results'
      condition: always()
      inputs:
        testResultsFormat: 'JUnit'
        testResultsFiles: '$(System.DefaultWorkingDirectory)/test-results/junit-pure.xml'
"""


def test_run_job_timeout_fires_and_still_publishes(tmp_path, capfd):
    # A hung step must be bounded by ``timeout``: the job reports failure (rc=1),
    # logs the timeout, and condition: always() stubs still collect results.
    pipeline = tmp_path / "azure-pipelines.yml"
    pipeline.write_text(_PURE_HANG_PIPELINE)
    rc = executor.run_job(str(pipeline), "pure_tests", no_container=True, timeout=1)
    assert rc == 1
    assert "timed out" in capfd.readouterr().out
    collected = tmp_path / "dev" / "build-out" / "results" / "junit-pure.xml"
    assert collected.exists()


# --------------------------------------------------------------------------
# E2-T5: opt-in real end-to-end runs (network + heavy toolchain)
# --------------------------------------------------------------------------
@pytest.mark.skipif(not os.environ.get("ADO_SANDBOX_E2E"),
                    reason="set ADO_SANDBOX_E2E=1 to run the real bare-host job")
def test_real_pure_tests_end_to_end():
    rc = executor.run_job(PIPELINE, "pure_tests", no_container=True)
    results = os.path.join(REPO_ROOT, "dev", "build-out", "results", "junit-pure.xml")
    assert rc == 0
    assert os.path.exists(results)


if __name__ == "__main__":
    raise SystemExit(pytest.main([__file__, "-v"]))