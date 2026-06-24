"""ado-sandbox: a dev-only reader/executor for the canonical ADO pipeline YAML.

Epic 1 provides the resolution core and the read-only ``--list`` / ``--dry-run``
CLI. Execution (Docker / bare-host) is added in later epics.
"""

import argparse
import os
import sys

from . import executor
from .loader import load_yaml
from .resolver import list_jobs, resolve_job


def _default_pipeline_path():
    repo_root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    return os.path.join(repo_root, "azure-pipelines.yml")


def _parse_var(values):
    overrides = {}
    for item in values or []:
        if "=" not in item:
            raise SystemExit("--var expects NAME=VALUE, got %r" % item)
        name, value = item.split("=", 1)
        overrides[name] = value
    return overrides


def _print_list(pipeline_path, stream):
    pipeline = load_yaml(pipeline_path)
    for stage in list_jobs(pipeline):
        label = stage["displayName"] or stage["stage"]
        stream.write("stage: %s (%s)\n" % (stage["stage"], label))
        for job in stage["jobs"]:
            display = job["displayName"] or job["job"]
            stream.write("  job: %s (%s)\n" % (job["job"], display))
            if job["condition"]:
                stream.write("    condition: %s\n" % job["condition"])


def _print_step(index, step, stream):
    header = "[%d] %s" % (index, step.kind)
    if step.kind == "checkout":
        header += ": %s" % step.repo
    elif step.kind == "task":
        header += ": %s" % step.task
    if step.display_name:
        header += " - %s" % step.display_name
    stream.write(header + "\n")
    if step.condition:
        stream.write("    condition: %s (-> %s)\n" % (step.condition, step.condition_result))
    if step.working_directory:
        stream.write("    workingDirectory: %s\n" % step.working_directory)
    if step.artifact:
        stream.write("    artifact: %s\n" % step.artifact)
    if step.source:
        stream.write("    download: %s\n" % step.source)
    if step.inputs:
        stream.write("    inputs:\n")
        for key, value in step.inputs.items():
            stream.write("      %s: %s\n" % (key, value))
    if step.env:
        for key, value in step.env.items():
            stream.write("    env %s=%s\n" % (key, value))
    if step.body is not None:
        for line in step.body.splitlines():
            stream.write("    | %s\n" % line)


def _print_dry_run(pipeline_path, job_name, cli_vars, stream):
    steps, job_condition = resolve_job(pipeline_path, job_name, cli_vars)
    stream.write("job: %s (condition -> %s)\n" % (job_name, job_condition))
    for index, step in enumerate(steps, start=1):
        _print_step(index, step, stream)


def main(argv=None):
    parser = argparse.ArgumentParser(prog="ado-sandbox")
    parser.add_argument("job", nargs="?", help="job key to resolve (e.g. pure_tests)")
    parser.add_argument("--list", action="store_true", help="list stages/jobs in the YAML")
    parser.add_argument("--dry-run", action="store_true", help="resolve + print steps (no Docker)")
    parser.add_argument("--no-container", action="store_true",
                        help="run the job bare-host (pure_tests, go_static_checks)")
    parser.add_argument("--var", action="append", default=[], metavar="NAME=VALUE",
                        help="override a runtime variable (highest precedence)")
    parser.add_argument("--pipeline", default=None, help="path to azure-pipelines.yml")
    args = parser.parse_args(argv)

    pipeline_path = args.pipeline or _default_pipeline_path()
    cli_vars = _parse_var(args.var)

    if args.list:
        _print_list(pipeline_path, sys.stdout)
        return 0
    if args.no_container:
        if not args.job:
            parser.error("--no-container requires a job argument")
        return executor.run_job(pipeline_path, args.job, cli_vars,
                                no_container=True, dry_run=args.dry_run)
    if args.dry_run:
        if not args.job:
            parser.error("--dry-run requires a job argument")
        _print_dry_run(pipeline_path, args.job, cli_vars, sys.stdout)
        return 0

    parser.error("nothing to do: pass --list or <job> --dry-run "
                 "(execution modes are added in later epics)")
