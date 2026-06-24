"""Task dispatch + ADO-only stubs for the ado-sandbox executor.

This module owns the non-``script``/``bash`` step kinds that the bare-host /
container executors do *not* fold into the assembled shell script:

* ``checkout`` -- resolved to existing local working copies (no network clone).
  ``self`` maps to the repository root; named repositories map to a sibling
  directory ``../<name>`` (mirroring ADO's side-by-side ``s/<repo>`` layout when
  more than one repository is checked out). ``submodules`` / ``fetchDepth`` /
  ``clean`` are accepted and logged but never fail-fast.
* ``DownloadPipelineArtifact@2`` -- stubbed: matching files are copied out of a
  developer-configured local cache (``dev/sandbox.yaml: artifact_cache.<name>``)
  into the target ``path``. A missing cache raises with an acquisition hint, or
  warns under ``--allow-missing-artifacts``. Never contacts Azure DevOps.
* ``publish`` / ``PublishTestResults@2`` / ``PublishCodeCoverageResults@2`` --
  stubbed: the referenced files are copied into ``dev/build-out/results/`` and
  logged. These never contact Azure DevOps.

Everything here is filesystem-only; no step ever performs a network or ADO API
call (Epic 2/3 acceptance criterion).
"""

import glob as _glob
import os
import shutil
import subprocess

from ._util import log as _log


class ArtifactCacheError(Exception):
    """Raised when a DownloadPipelineArtifact@2 cache is absent (E3-T2).

    Carries a precise acquisition hint so the developer knows exactly how to
    populate the missing artifact (or to re-run with ``--allow-missing-artifacts``).
    """


def _split_result_files(value):
    """Split an ADO ``testResultsFiles`` value into individual paths.

    ADO accepts either a single path or a newline-separated multi-line list.
    """
    if value is None:
        return []
    return [line.strip() for line in str(value).splitlines() if line.strip()]


def plan_checkouts(checkout_steps, repo_root, stream):
    """Map ``checkout`` steps to local directories and pick the job working dir.

    Returns ``(working_dir, mapping)`` where ``mapping`` is ``repo -> local
    path``. With a single checkout the sources live directly in the working
    directory (ADO's ``s/``); with multiple checkouts each repository lives in
    its own sibling sub-directory and the working directory is their parent.

    Never clones and never fails: a missing local working copy is only logged.
    """
    real_checkouts = [s for s in checkout_steps if s.repo not in (None, "none")]
    multi = len(real_checkouts) > 1
    working_dir = os.path.dirname(repo_root) if multi else repo_root

    mapping = {}
    for step in real_checkouts:
        repo = step.repo
        if repo == "self":
            path = os.path.join(working_dir, os.path.basename(repo_root)) if multi else repo_root
        else:
            path = os.path.join(os.path.dirname(repo_root), repo)
        mapping[repo] = path
        if os.path.isdir(path):
            _log(stream, "checkout %s -> %s" % (repo, path))
        else:
            _log(stream, "checkout %s -> %s (WARNING: local working copy not found, "
                         "continuing)" % (repo, path))
    return working_dir, mapping


def _collect_files(paths, results_dir, label, stream):
    os.makedirs(results_dir, exist_ok=True)
    for path in paths:
        if path and os.path.isfile(path):
            dest = os.path.join(results_dir, os.path.basename(path))
            shutil.copy2(path, dest)
            _log(stream, "%s: collected %s -> %s" % (label, path, dest))
        else:
            _log(stream, "%s: %r not found, skipping (never fails)" % (label, path))


def _collect_tree(src_dir, dest_dir, label, stream):
    """Recursively copy every file under ``src_dir`` into ``dest_dir``.

    Used by the build-deb ``publish:`` stub to collect the produced ``.deb``
    files (and any other staged outputs) into ``dev/build-out/<artifact>/``,
    preserving the staging directory's relative layout. Never fails.
    """
    os.makedirs(dest_dir, exist_ok=True)
    copied = 0
    for root, _dirs, files in os.walk(src_dir):
        for name in files:
            src = os.path.join(root, name)
            rel = os.path.relpath(src, src_dir)
            dest = os.path.join(dest_dir, rel)
            os.makedirs(os.path.dirname(dest) or dest_dir, exist_ok=True)
            shutil.copy2(src, dest)
            copied += 1
            _log(stream, "%s: collected %s -> %s" % (label, src, dest))
    if copied == 0:
        _log(stream, "%s: no files under %s, skipping (never fails)"
                     % (label, src_dir))


def _handle_publish(step, results_dir, stream):
    """Run a ``publish:`` stub: collect the referenced path locally (never ADO).

    A *file* body (e.g. ``publish: .../coverage.xml``) is copied into
    ``results_dir`` (``dev/build-out/results/``), exactly as in Epic 2. A
    *directory* body (the build-deb ``publish: $(Build.ArtifactStagingDirectory)/``
    shorthand for artifact upload) is collected into
    ``dev/build-out/<artifact>/`` so the produced ``.deb`` files land under
    ``dev/build-out/`` (E4-T1).
    """
    source = step.body
    if source and os.path.isdir(source):
        build_out = os.path.dirname(os.path.normpath(results_dir))
        artifact = step.artifact or "artifact"
        dest_dir = os.path.join(build_out, artifact)
        _collect_tree(source, dest_dir, "publish (%s)" % artifact, stream)
    else:
        _collect_files([source], results_dir, "publish", stream)


def handle_stub_step(step, results_dir, stream):
    """Run a publish/result-publishing stub: copy files into ``results_dir``.

    Supports ``publish``, ``PublishTestResults@2`` and
    ``PublishCodeCoverageResults@2``. ``DownloadPipelineArtifact@2`` is fleshed
    out in a later epic and is only logged here. Never contacts ADO.
    """
    if step.kind == "publish":
        _handle_publish(step, results_dir, stream)
        return

    task = step.task
    inputs = step.inputs or {}
    if task == "PublishTestResults@2":
        files = _split_result_files(inputs.get("testResultsFiles"))
        _collect_files(files, results_dir, "PublishTestResults@2", stream)
    elif task == "PublishCodeCoverageResults@2":
        files = _split_result_files(inputs.get("summaryFileLocation"))
        _collect_files(files, results_dir, "PublishCodeCoverageResults@2", stream)
    elif task == "DownloadPipelineArtifact@2":
        # In container execution the download is performed *before* the script
        # (see executor.download_artifacts); reaching it here means a bare-host
        # job referenced it -- log and skip rather than touch the network.
        artifact = (step.inputs or {}).get("artifact")
        _log(stream, "DownloadPipelineArtifact@2: artifact %r handled pre-script "
                     "(no ADO call)" % artifact)
    else:
        _log(stream, "unhandled stub task %r, skipping" % task)


def _split_patterns(value):
    """Split an ADO ``patterns`` input (newline/multi-line globs) into a list."""
    if value is None:
        return []
    return [line.strip() for line in str(value).splitlines() if line.strip()]


def _artifact_hint(artifact, cache_dir):
    """Return the precise acquisition hint for a missing artifact cache."""
    location = cache_dir or "dev/sandbox.yaml: artifact_cache.%s" % artifact
    return (
        "DownloadPipelineArtifact@2: artifact %r is not available locally "
        "(expected cache dir: %s).\n"
        "  Populate it once with the ADO CLI, e.g.:\n"
        "    az pipelines runs artifact download --artifact-name %s "
        "--path %s --run-id <RUN_ID>\n"
        "  or build it from source (see dev/README.md), then point "
        "dev/sandbox.yaml: artifact_cache.%s at that directory.\n"
        "  Re-run with --allow-missing-artifacts to downgrade this to a warning."
        % (artifact, location, artifact, location, artifact)
    )


def download_artifact(step, config, default_path, stream, allow_missing=False):
    """Stub ``DownloadPipelineArtifact@2``: copy cached files into ``path``.

    Reads ``inputs.artifact`` / ``inputs.patterns`` / ``inputs.path`` from the
    resolved step, copies matching files out of
    ``config['artifact_cache'][artifact]`` (preserving their relative layout)
    into the target path (``inputs.path`` or ``default_path``, ADO's
    ``$(Pipeline.Workspace)``). With no ``patterns`` the whole cache is copied.

    Returns ``True`` when files were copied, ``False`` when the cache was absent
    and ``allow_missing`` downgraded it to a warning. Raises
    :class:`ArtifactCacheError` (with an acquisition hint) when the cache is
    absent and ``allow_missing`` is not set. Never contacts ADO.
    """
    inputs = step.inputs or {}
    artifact = inputs.get("artifact")
    target = inputs.get("path") or default_path
    cache_dir = (config.get("artifact_cache") or {}).get(artifact)

    if not cache_dir or not os.path.isdir(cache_dir):
        hint = _artifact_hint(artifact, cache_dir)
        if allow_missing:
            _log(stream, "WARNING: %s" % hint)
            return False
        raise ArtifactCacheError(hint)

    patterns = _split_patterns(inputs.get("patterns"))
    matches = []
    if patterns:
        for pattern in patterns:
            matches.extend(_glob.glob(os.path.join(cache_dir, pattern)))
    else:
        for root, _dirs, files in os.walk(cache_dir):
            for name in files:
                matches.append(os.path.join(root, name))

    os.makedirs(target, exist_ok=True)
    copied = 0
    for src in sorted(set(matches)):
        if not os.path.isfile(src):
            continue
        rel = os.path.relpath(src, cache_dir)
        dest = os.path.join(target, rel)
        os.makedirs(os.path.dirname(dest) or target, exist_ok=True)
        shutil.copy2(src, dest)
        copied += 1
        _log(stream, "DownloadPipelineArtifact@2 (%s): %s -> %s"
                     % (artifact, src, dest))
    if copied == 0:
        _log(stream, "DownloadPipelineArtifact@2 (%s): no files matched %r in %s"
                     % (artifact, patterns or "<all>", cache_dir))
    return True


# ---------------------------------------------------------------------------
# E4-T3: optional, gated ``fetch-artifacts`` helper
# ---------------------------------------------------------------------------
# These are the three ADO-only DownloadPipelineArtifact@2 artifacts (no public
# download URL); the cache keys in dev/sandbox.yaml match their ADO names.
_FETCH_ENV_CRED_VARS = ("AZURE_DEVOPS_EXT_PAT", "SYSTEM_ACCESSTOKEN")


def _az_available():
    return bool(shutil.which("az"))


def _az_creds_present(environ):
    return any(environ.get(name) for name in _FETCH_ENV_CRED_VARS)


def _fetch_instructions(caches, run_id):
    lines = [
        "fetch-artifacts: the Azure CLI (`az`) with a logged-in / PAT credential "
        "is required to download the ADO-only artifacts automatically.",
        "  Install it (https://aka.ms/azure-cli) and the devops extension "
        "(`az extension add --name azure-devops`), export a PAT as "
        "AZURE_DEVOPS_EXT_PAT, then re-run with --run-id <RUN_ID>.",
        "  Or download each artifact manually:",
    ]
    for artifact, dest in caches.items():
        lines.append(
            "    az pipelines runs artifact download --artifact-name %s "
            "--path %s --run-id %s" % (artifact, dest, run_id or "<RUN_ID>"))
    lines.append(
        "  Or build them from source and copy the files into the cache dirs "
        "(see dev/README.md).")
    return "\n".join(lines)


def _default_az_runner(cmd):
    subprocess.run(cmd, check=True)


def fetch_artifacts(config, stream, run_id=None, az_runner=None, environ=None):
    """Optionally download the three ADO-only artifacts into the cache (E4-T3).

    Gated and optional: an explicit ``az_runner`` (used by tests) bypasses the
    gate, otherwise this only shells out to ``az`` when the CLI is installed
    *and* an ADO credential is present in the environment. When the gate is not
    satisfied -- or no ``--run-id`` is given -- it prints precise manual
    acquisition instructions and returns ``False`` without touching the network.

    Returns ``True`` when artifacts were downloaded, ``False`` otherwise.
    """
    environ = os.environ if environ is None else environ
    caches = config.get("artifact_cache") or {}
    if not caches:
        _log(stream, "fetch-artifacts: no artifact_cache configured in "
                     "dev/sandbox.yaml; nothing to do")
        return False

    if az_runner is None and not (_az_available() and _az_creds_present(environ)):
        _log(stream, _fetch_instructions(caches, run_id))
        return False

    if run_id is None:
        _log(stream, "fetch-artifacts: --run-id <RUN_ID> is required to download "
                     "via az.\n" + _fetch_instructions(caches, run_id))
        return False

    runner = az_runner or _default_az_runner
    for artifact, dest in caches.items():
        os.makedirs(dest, exist_ok=True)
        cmd = ["az", "pipelines", "runs", "artifact", "download",
               "--artifact-name", artifact, "--path", dest,
               "--run-id", str(run_id)]
        _log(stream, "fetch-artifacts: %s" % " ".join(cmd))
        runner(cmd)
        _log(stream, "fetch-artifacts: %s -> %s" % (artifact, dest))
    return True
