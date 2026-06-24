"""Task dispatch + ADO-only stubs for the ado-sandbox executor.

This module owns the non-``script``/``bash`` step kinds that the bare-host /
container executors do *not* fold into the assembled shell script:

* ``checkout`` -- resolved to existing local working copies (no network clone).
  ``self`` maps to the repository root; named repositories map to a sibling
  directory ``../<name>`` (mirroring ADO's side-by-side ``s/<repo>`` layout when
  more than one repository is checked out). ``submodules`` / ``fetchDepth`` /
  ``clean`` are accepted and logged but never fail-fast.
* ``publish`` / ``PublishTestResults@2`` / ``PublishCodeCoverageResults@2`` --
  stubbed: the referenced files are copied into ``dev/build-out/results/`` and
  logged. These never contact Azure DevOps.

Everything here is filesystem-only; no step ever performs a network or ADO API
call (Epic 2 acceptance criterion).
"""

import os
import shutil

from ._util import log as _log


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


def handle_stub_step(step, results_dir, stream):
    """Run a publish/result-publishing stub: copy files into ``results_dir``.

    Supports ``publish``, ``PublishTestResults@2`` and
    ``PublishCodeCoverageResults@2``. ``DownloadPipelineArtifact@2`` is fleshed
    out in a later epic and is only logged here. Never contacts ADO.
    """
    if step.kind == "publish":
        _collect_files([step.body], results_dir, "publish", stream)
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
        _log(stream, "DownloadPipelineArtifact@2: artifact %r stubbed (handled in a "
                     "later epic, no ADO call)" % step.artifact)
    else:
        _log(stream, "unhandled stub task %r, skipping" % task)
