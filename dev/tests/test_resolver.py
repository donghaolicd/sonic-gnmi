"""Golden / unit tests for the ado-sandbox resolution core (Epic 1).

Run with: ``python3 -m pytest dev/tests/test_resolver.py`` (or plain
``python3 dev/tests/test_resolver.py`` for a lightweight self-check).

These tests exercise pure resolution only -- no Docker, no artifacts (NFR4).
"""

import hashlib
import io
import os
import sys

import pytest

DEV_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
REPO_ROOT = os.path.dirname(DEV_DIR)
sys.path.insert(0, DEV_DIR)

import ado_sandbox  # noqa: E402
from ado_sandbox import loader, resolver  # noqa: E402

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


if __name__ == "__main__":
    raise SystemExit(pytest.main([__file__, "-v"]))
