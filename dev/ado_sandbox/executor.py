"""Executor: run resolved jobs locally (Epic 2: bare-host ``--no-container``).

The executor mirrors ADO's "all steps of a job share one agent": it concatenates
the resolved ``script`` / ``bash`` step bodies into a *single* bash script
(``set -e``, per-step ``echo "##[section]<displayName>"`` markers, per-step
``workingDirectory`` / ``env``) so in-job state (PATH exports, installed
toolchains) is preserved across steps. ``checkout`` steps are resolved to local
working copies and the publish/result tasks are stubbed -- both via
``tasks.py`` -- and never touch the network.

For Epic 2 only the bare-host tier (``--no-container``) is executed; the
container tier is added in a later epic.
"""

import os
import re
import shlex
import subprocess
import sys
import tempfile
import io

from . import tasks
from ._util import log as _log
from .resolver import ResolverError, resolve_job

_SCRIPT_KINDS = ("script", "bash")
_STUB_KINDS = ("publish", "task")

# Default per-job wall-clock timeout (seconds) so a hung step (e.g. one
# blocking on stdin) cannot wedge the developer tool indefinitely.
_DEFAULT_TIMEOUT = 1800

# A valid POSIX shell environment variable name.
_ENV_KEY_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def _quote(value):
    return shlex.quote(str(value))


def assemble_script(steps, working_dir):
    """Concatenate ``script``/``bash`` step bodies into one bash script.

    Each step is preceded by an ``echo "##[section]<name>"`` marker, reset to
    the job ``working_dir`` (then into its own ``workingDirectory`` if set), and
    has its ``env`` exported. ``set -e`` aborts the job on the first failure,
    reproducing ADO's default ``succeeded()`` gating.
    """
    lines = ["#!/usr/bin/env bash", "set -e", ""]
    for step in steps:
        if step.kind not in _SCRIPT_KINDS or step.body is None:
            continue
        name = step.display_name or step.kind
        lines.append('echo "##[section]%s"' % name)
        lines.append("cd %s" % _quote(working_dir))
        if step.working_directory:
            lines.append("cd %s" % _quote(step.working_directory))
        for key, value in (step.env or {}).items():
            if not _ENV_KEY_RE.match(str(key)):
                raise ResolverError(
                    "invalid env variable name %r in step %r"
                    % (key, step.display_name or step.kind)
                )
            lines.append("export %s=%s" % (key, _quote(value)))
        lines.append(step.body.rstrip("\n"))
        lines.append("")
    return "\n".join(lines)


def _should_run_stub(step, failed, stream):
    """Apply ADO ``condition:`` execution semantics to a deferred stub step.

    ``always()`` always runs; ``failed()`` runs only after a prior failure;
    ``succeeded()`` (and no condition) runs only when nothing has failed yet.
    Anything non-trivial defaults to running, with a logged note (FR7).
    """
    cond = (step.condition or "").strip()
    if not cond:
        return not failed
    low = cond.lower()
    if "always()" in low:
        return True
    if "succeeded()" in low:
        return not failed
    if "failed()" in low:
        return failed
    _log(stream, "condition %r not recognised, defaulting to run" % cond)
    return True


def run_job(pipeline_path, job_name, cli_vars=None, no_container=False,
            dry_run=False, repo_root=None, results_dir=None, stream=None,
            timeout=_DEFAULT_TIMEOUT):
    """Resolve and run ``job_name`` from ``pipeline_path`` on the bare host.

    Returns ``0`` when the job succeeded (or was skipped/dry-run) and ``1`` when
    the assembled script exited non-zero. With ``dry_run`` the assembled script
    is printed and nothing is executed (NFR4: runnable with no Docker and no
    artifacts). ``timeout`` bounds the assembled script's wall-clock runtime.
    """
    stream = stream or sys.stdout
    cli_vars = dict(cli_vars or {})

    if repo_root is None:
        repo_root = os.path.dirname(os.path.abspath(str(pipeline_path)))
    if results_dir is None:
        results_dir = os.path.join(repo_root, "dev", "build-out", "results")

    if not no_container:
        raise NotImplementedError(
            "container execution is added in a later epic; use --no-container"
        )

    # Pass 1: resolve to discover checkout steps and the job working directory.
    steps, _ = resolve_job(pipeline_path, job_name, cli_vars)
    checkout_steps = [s for s in steps if s.kind == "checkout"]
    working_dir, _ = tasks.plan_checkouts(checkout_steps, repo_root, io.StringIO())

    # Pass 2: re-resolve with bare-host pseudo-paths so result-file macros such
    # as $(System.DefaultWorkingDirectory) point at the real local tree.
    staging_dir = os.path.join(repo_root, "dev", "build-out", "staging")
    overrides = {
        "System.DefaultWorkingDirectory": working_dir,
        "Pipeline.Workspace": working_dir,
        "Build.ArtifactStagingDirectory": staging_dir,
    }
    overrides.update(cli_vars)
    steps, job_condition = resolve_job(pipeline_path, job_name, overrides)

    if not job_condition:
        _log(stream, "job %s condition is false; skipping" % job_name)
        return 0

    checkout_steps = [s for s in steps if s.kind == "checkout"]
    tasks.plan_checkouts(checkout_steps, repo_root, stream)

    script_steps = []
    for step in steps:
        if step.kind in _SCRIPT_KINDS:
            cond = (step.condition or "").lower()
            if "failed()" in cond:
                _log(stream, "skipping step %r: failed() condition cannot run in a "
                             "pre-assembled script" % (step.display_name or step.kind))
                continue
            script_steps.append(step)

    script = assemble_script(script_steps, working_dir)

    if dry_run:
        stream.write(script)
        if not script.endswith("\n"):
            stream.write("\n")
        return 0

    failed = False
    handle, path = tempfile.mkstemp(prefix="ado-sandbox-", suffix=".sh")
    try:
        with os.fdopen(handle, "w") as script_file:
            script_file.write(script)
        _log(stream, "running job %s (bare-host) in %s" % (job_name, working_dir))
        stream.flush()
        try:
            result = subprocess.run(["bash", path], cwd=working_dir, timeout=timeout)
            failed = result.returncode != 0
            if failed:
                _log(stream, "job %s script exited with code %d" % (job_name, result.returncode))
        except subprocess.TimeoutExpired:
            failed = True
            _log(stream, "job %s timed out after %ss" % (job_name, timeout))
    finally:
        os.remove(path)

    # Deferred stubs (publish / result publishing) honour condition: semantics
    # so condition: always() collects results even after a failed script.
    for step in steps:
        if step.kind in _STUB_KINDS and _should_run_stub(step, failed, stream):
            tasks.handle_stub_step(step, results_dir, stream)

    return 1 if failed else 0
