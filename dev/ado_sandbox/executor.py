"""Executor: run resolved jobs locally (Epic 2: bare-host ``--no-container``).

The executor mirrors ADO's "all steps of a job share one agent": it concatenates
the resolved ``script`` / ``bash`` step bodies into a *single* bash script
(``set -e``, per-step ``echo "##[section]<displayName>"`` markers, per-step
``workingDirectory`` / ``env``) so in-job state (PATH exports, installed
toolchains) is preserved across steps. ``checkout`` steps are resolved to local
working copies and the publish/result tasks are stubbed -- both via
``tasks.py`` -- and never touch the network.

For Epic 2 the bare-host tier (``--no-container``) is executed; Epic 3 adds the
container tier (default): the same assembled script is run inside the SONiC
slave image via a single ``docker run`` per job, the three ADO-only artifacts
are copied out of a local cache before the script, and ``--shell`` drops the
developer into an interactive container shell.
"""

import collections
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

# Container hardening defaults: the SONiC slave image opens many fds during a
# full build, so raise the soft/hard nofile limit (mirrors ADO's agent host).
_DEFAULT_ULIMITS = ("nofile=1048576:1048576",)

# Proxy variables forwarded into the container so apt/pip/dpkg can reach mirrors
# behind a corporate proxy, exactly as the ADO agent forwards them.
_PROXY_ENV_VARS = (
    "http_proxy", "https_proxy", "no_proxy", "ftp_proxy",
    "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "FTP_PROXY",
)

# Path the assembled job script is mounted at inside the container.
_CONTAINER_SCRIPT_PATH = "/ado-sandbox-job.sh"


# A fully-resolved container job ready to hand to ``docker run`` (E3-T1/T4).
ContainerPlan = collections.namedtuple(
    "ContainerPlan",
    "image working_dir mounts env ulimits script "
    "download_steps publish_steps results_dir staging_dir job_condition",
)


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


def _proxy_env(environ=None):
    """Return the proxy env vars present in ``environ`` to forward (E3-T1)."""
    environ = os.environ if environ is None else environ
    return {name: environ[name] for name in _PROXY_ENV_VARS if environ.get(name)}


def build_docker_command(image, mounts, env, ulimits, working_dir,
                         script_path=None, interactive=False):
    """Build a single ``docker run`` argv for one job (E3-T1/T4, DD6).

    ``mounts`` is a list of ``(host, container, mode)`` tuples; ``env`` a dict of
    extra environment variables; ``ulimits`` a list of ``docker --ulimit``
    values. With ``interactive`` the container drops into ``/bin/bash`` (``-it``)
    instead of running ``script_path``. One ``docker run`` per job preserves
    in-job state (PATH exports, installed debs) exactly as ADO's single agent.
    """
    cmd = ["docker", "run", "--rm"]
    if interactive:
        cmd.append("-it")
    for host, container, mode in mounts:
        spec = "%s:%s" % (host, container)
        if mode:
            spec += ":%s" % mode
        cmd += ["-v", spec]
    for key, value in env.items():
        cmd += ["-e", "%s=%s" % (key, value)]
    for limit in ulimits:
        cmd += ["--ulimit", limit]
    cmd += ["-w", working_dir, image]
    if interactive:
        cmd.append("/bin/bash")
    else:
        cmd += ["bash", script_path]
    return cmd


def _container_mounts(working_dir, config):
    """Mount the job working dir (repo + siblings + results + staging) rw, and
    each existing artifact cache dir ro (E3-T1)."""
    mounts = [(working_dir, working_dir, "rw")]
    for cache_dir in (config.get("artifact_cache") or {}).values():
        if cache_dir and os.path.isdir(cache_dir) and cache_dir != working_dir:
            mounts.append((cache_dir, cache_dir, "ro"))
    return mounts


def _script_steps(steps, stream):
    """Select the ``script``/``bash`` steps that can run in a pre-assembled
    script (``failed()`` steps cannot and are skipped, as in the bare-host tier).
    """
    selected = []
    for step in steps:
        if step.kind in _SCRIPT_KINDS:
            cond = (step.condition or "").lower()
            if "failed()" in cond:
                _log(stream, "skipping step %r: failed() condition cannot run in a "
                             "pre-assembled script" % (step.display_name or step.kind))
                continue
            selected.append(step)
    return selected


def prepare_container_job(pipeline_path, job_name, config, cli_vars=None,
                          repo_root=None, stream=None):
    """Resolve ``job_name`` into an executable :class:`ContainerPlan` (E3-T1).

    Performs the same two-pass resolution as the bare-host tier so result-file
    and artifact macros (``$(System.DefaultWorkingDirectory)``,
    ``$(Pipeline.Workspace)``, ``$(Build.ArtifactStagingDirectory)``) point at
    the real local tree, then assembles the container mounts, env, ulimits and
    the concatenated job script. Pure planning: no Docker, no copies, no ADO.
    """
    stream = stream or sys.stdout
    cli_vars = dict(cli_vars or {})
    if repo_root is None:
        repo_root = os.path.dirname(os.path.abspath(str(pipeline_path)))
    results_dir = os.path.join(repo_root, "dev", "build-out", "results")
    staging_dir = os.path.join(repo_root, "dev", "build-out", "staging")

    # Pass 1: discover checkouts + the job working directory.
    steps, _ = resolve_job(pipeline_path, job_name, cli_vars)
    checkout_steps = [s for s in steps if s.kind == "checkout"]
    working_dir, _ = tasks.plan_checkouts(checkout_steps, repo_root, io.StringIO())

    # Pass 2: re-resolve with sandbox mount paths.
    overrides = {
        "System.DefaultWorkingDirectory": working_dir,
        "Pipeline.Workspace": working_dir,
        "Build.ArtifactStagingDirectory": staging_dir,
    }
    overrides.update(cli_vars)
    steps, job_condition = resolve_job(pipeline_path, job_name, overrides)

    download_steps = [s for s in steps
                      if s.kind == "task" and s.task == "DownloadPipelineArtifact@2"]
    publish_steps = [s for s in steps
                     if s.kind in _STUB_KINDS and s not in download_steps]
    script = assemble_script(_script_steps(steps, stream), working_dir)

    return ContainerPlan(
        image=config.get("image"),
        working_dir=working_dir,
        mounts=_container_mounts(working_dir, config),
        env=_proxy_env(),
        ulimits=list(_DEFAULT_ULIMITS),
        script=script,
        download_steps=download_steps,
        publish_steps=publish_steps,
        results_dir=results_dir,
        staging_dir=staging_dir,
        job_condition=job_condition,
    )


def download_artifacts(plan, config, stream, allow_missing=False):
    """Run every DownloadPipelineArtifact@2 stub for ``plan`` before the script.

    The default download target is ``$(Pipeline.Workspace)`` -> the job working
    directory (mirroring ADO). Raises :class:`tasks.ArtifactCacheError` on a
    missing cache unless ``allow_missing`` downgrades it to a warning.
    """
    for step in plan.download_steps:
        tasks.download_artifact(step, config, plan.working_dir, stream,
                                allow_missing=allow_missing)


def run_shell(pipeline_path, job_name, config, cli_vars=None, repo_root=None,
              stream=None, allow_missing_artifacts=True, dry_run=False):
    """Drop the developer into an interactive container shell for ``job_name``.

    Resolves env + mounts (and pre-fetches the artifact cache, best-effort) then
    runs ``docker run -it ... /bin/bash`` in the prepared container instead of
    the assembled script (E3-T4). With ``dry_run`` the ``docker`` argv is printed
    and returned without executing -- so the mode is testable without Docker.
    """
    stream = stream or sys.stdout
    plan = prepare_container_job(pipeline_path, job_name, config, cli_vars,
                                 repo_root, stream)
    download_artifacts(plan, config, stream, allow_missing=allow_missing_artifacts)
    cmd = build_docker_command(plan.image, plan.mounts, plan.env, plan.ulimits,
                               plan.working_dir, interactive=True)
    if dry_run:
        stream.write(" ".join(shlex.quote(part) for part in cmd) + "\n")
        return cmd
    _log(stream, "entering interactive shell for job %s (image %s)"
                 % (job_name, plan.image))
    stream.flush()
    return subprocess.call(cmd)


def _run_container_job(pipeline_path, job_name, config, cli_vars, repo_root,
                       results_dir, stream, timeout, allow_missing_artifacts,
                       dry_run):
    """Execute ``job_name`` inside the SONiC slave container (E3-T1)."""
    plan = prepare_container_job(pipeline_path, job_name, config, cli_vars,
                                 repo_root, stream)
    if results_dir is not None:
        plan = plan._replace(results_dir=results_dir)

    if not plan.job_condition:
        _log(stream, "job %s condition is false; skipping" % job_name)
        return 0

    if dry_run:
        cmd = build_docker_command(plan.image, plan.mounts, plan.env, plan.ulimits,
                                   plan.working_dir, _CONTAINER_SCRIPT_PATH)
        stream.write(" ".join(shlex.quote(part) for part in cmd) + "\n")
        stream.write(plan.script)
        if not plan.script.endswith("\n"):
            stream.write("\n")
        return 0

    # Re-plan checkouts so missing sibling working copies are logged to the
    # caller's stream (planning above used a throwaway stream).
    checkout_steps = []
    steps_resolved, _ = resolve_job(pipeline_path, job_name, dict(cli_vars or {}))
    checkout_steps = [s for s in steps_resolved if s.kind == "checkout"]
    tasks.plan_checkouts(checkout_steps, repo_root or os.path.dirname(
        os.path.abspath(str(pipeline_path))), stream)

    # Artifacts must be present before the install scripts dpkg -i them.
    download_artifacts(plan, config, stream, allow_missing=allow_missing_artifacts)

    failed = False
    handle, host_script = tempfile.mkstemp(prefix="ado-sandbox-", suffix=".sh")
    try:
        with os.fdopen(handle, "w") as script_file:
            script_file.write(plan.script)
        mounts = plan.mounts + [(host_script, _CONTAINER_SCRIPT_PATH, "ro")]
        cmd = build_docker_command(plan.image, mounts, plan.env, plan.ulimits,
                                   plan.working_dir, _CONTAINER_SCRIPT_PATH)
        _log(stream, "running job %s in container %s (%s)"
                     % (job_name, plan.image, plan.working_dir))
        stream.flush()
        try:
            result = subprocess.run(cmd, timeout=timeout)
            failed = result.returncode != 0
            if failed:
                _log(stream, "job %s container exited with code %d"
                             % (job_name, result.returncode))
        except subprocess.TimeoutExpired:
            failed = True
            _log(stream, "job %s timed out after %ss" % (job_name, timeout))
    finally:
        os.remove(host_script)

    for step in plan.publish_steps:
        if _should_run_stub(step, failed, stream):
            tasks.handle_stub_step(step, plan.results_dir, stream)

    return 1 if failed else 0


def run_job(pipeline_path, job_name, cli_vars=None, no_container=False,
            dry_run=False, repo_root=None, results_dir=None, stream=None,
            timeout=_DEFAULT_TIMEOUT, config=None, allow_missing_artifacts=False):
    """Resolve and run ``job_name`` from ``pipeline_path``.

    Bare-host (``no_container=True``) assembles the step bodies into one local
    bash script; the container tier (default) runs the same assembled script
    inside the SONiC slave image via a single ``docker run`` (E3-T1). Returns
    ``0`` on success/skip/dry-run and ``1`` on failure. With ``dry_run`` nothing
    is executed (NFR4). ``timeout`` bounds the wall-clock runtime.
    """
    stream = stream or sys.stdout
    cli_vars = dict(cli_vars or {})

    if repo_root is None:
        repo_root = os.path.dirname(os.path.abspath(str(pipeline_path)))
    if results_dir is None:
        results_dir = os.path.join(repo_root, "dev", "build-out", "results")

    if not no_container:
        if config is None:
            config = {"image": "sonic-slave-trixie:local"}
        return _run_container_job(pipeline_path, job_name, config, cli_vars,
                                  repo_root, results_dir, stream, timeout,
                                  allow_missing_artifacts, dry_run)
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
