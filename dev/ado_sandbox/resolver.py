"""ADO YAML resolver: turns a raw pipeline AST into a flat list of steps.

Three layers of resolution are applied (see ``dev/ado-sandbox.plan.md``):

1. ``${{ }}`` compile-time layer -- conditional insertion
   (``${{ if }}`` / ``${{ elseif }}`` / ``${{ else }}``) plus inline
   substitution of ``${{ parameters.x }}`` / ``${{ variables.x }}``. Backed by a
   restricted expression grammar: string/bool/number literals, dotted access
   (``parameters.arch``), bracket-index access (``variables['Build.Reason']``),
   and the functions ``eq``, ``ne``, ``and``, ``or``, ``not``.

2. Template expansion -- a ``{template: path, parameters: {...}}`` step loads
   the referenced template (relative to the including file), overlays the
   passed parameters onto the template defaults, and recurses.

3. ``$(VAR)`` runtime macro layer (ADO-faithful, DD7) -- only ``$(Name)`` tokens
   whose ``Name`` matches a known variable are substituted; every other
   ``$(...)`` (shell command substitution, nested forms) is preserved verbatim.
   Substitution is innermost-first.

Plus a ``condition:`` layer (FR7): ``always()`` / ``succeeded()`` / ``failed()``
and known-operand expressions are evaluated; unknown-operand conditions default
to ``True``. ``condition:`` and unresolved ``$()`` never fail-fast; unsupported
*structural* constructs do (NFR3).
"""

import os
import re
from dataclasses import dataclass, field

from .loader import load_yaml


class ResolverError(Exception):
    """Raised on an unsupported structural ADO construct (NFR3)."""


# ---------------------------------------------------------------------------
# Built-in pseudo-variable table (System.* / Build.* / Pipeline.*)
# ---------------------------------------------------------------------------
DEFAULT_PSEUDO_VARS = {
    "System.DefaultWorkingDirectory": "/sandbox",
    "System.PullRequest.TargetBranch": "master",
    "Pipeline.Workspace": "/sandbox",
    "Build.ArtifactStagingDirectory": "/sandbox/staging",
    "Build.SourceBranchName": "master",
    "Build.Reason": "PullRequest",
    "BUILD_BRANCH": "master",
    "GO_VERSION": "1.24.4",
}

# Step keys we understand. Order matters only for kind detection.
_STEP_KIND_KEYS = ("script", "bash", "checkout", "task", "publish", "download", "template")

# Known ADO tasks (``- task: Name@N``). Anything else fails-fast (NFR3).
KNOWN_TASKS = {
    "DownloadPipelineArtifact@2",
    "PublishTestResults@2",
    "PublishCodeCoverageResults@2",
}


@dataclass
class Step:
    kind: str
    display_name: str = None
    body: str = None
    task: str = None
    inputs: dict = None
    repo: str = None
    source: str = None
    artifact: str = None
    working_directory: str = None
    env: dict = field(default_factory=dict)
    condition: str = None
    condition_result: bool = True
    file: str = None
    line: int = None


# ===========================================================================
# Restricted ``${{ }}`` expression evaluator
# ===========================================================================
class _Unknown(Exception):
    """Internal: an operand could not be resolved (used in lenient mode)."""


_TOKEN_RE = re.compile(
    r"""(?:
          (?P<string>'[^']*'|"[^"]*")
        | (?P<number>\d+)
        | (?P<lparen>\()
        | (?P<rparen>\))
        | (?P<lbracket>\[)
        | (?P<rbracket>\])
        | (?P<comma>,)
        | (?P<dot>\.)
        | (?P<ident>[A-Za-z_][A-Za-z0-9_]*)
        )""",
    re.VERBOSE,
)

_BOOL_LITERALS = {"true": True, "false": False, "True": True, "False": False}


def _tokenize(expr):
    tokens = []
    pos = 0
    while pos < len(expr):
        if expr[pos].isspace():
            pos += 1
            continue
        match = _TOKEN_RE.match(expr, pos)
        if not match or match.end() == pos:
            raise ResolverError("cannot tokenize expression near %r" % expr[pos:])
        kind = match.lastgroup
        value = match.group(kind)
        tokens.append((kind, value))
        pos = match.end()
    return tokens


class _ExprParser:
    def __init__(self, tokens, ctx, lenient):
        self.tokens = tokens
        self.pos = 0
        self.ctx = ctx
        self.lenient = lenient

    def _peek(self):
        return self.tokens[self.pos] if self.pos < len(self.tokens) else (None, None)

    def _next(self):
        token = self._peek()
        self.pos += 1
        return token

    def parse(self):
        value = self._parse_primary()
        if self.pos != len(self.tokens):
            raise ResolverError("trailing tokens in expression")
        return value

    def _parse_primary(self):
        kind, value = self._next()
        if kind == "string":
            return value[1:-1]
        if kind == "number":
            return int(value)
        if kind == "ident":
            if self._peek()[0] == "lparen":
                return self._parse_call(value)
            if value in _BOOL_LITERALS:
                return _BOOL_LITERALS[value]
            return self._parse_reference(value)
        raise ResolverError("unexpected token %r in expression" % (value,))

    def _parse_call(self, name):
        self._next()  # consume '('
        args = []
        if self._peek()[0] != "rparen":
            args.append(self._parse_primary())
            while self._peek()[0] == "comma":
                self._next()
                args.append(self._parse_primary())
        if self._next()[0] != "rparen":
            raise ResolverError("missing ')' in call to %s()" % name)
        return _apply_function(name, args)

    def _parse_reference(self, root):
        keys = []
        while True:
            kind, _ = self._peek()
            if kind == "dot":
                self._next()
                ident_kind, ident_value = self._next()
                if ident_kind != "ident":
                    raise ResolverError("expected identifier after '.'")
                keys.append(ident_value)
            elif kind == "lbracket":
                self._next()
                str_kind, str_value = self._next()
                if str_kind != "string":
                    raise ResolverError("expected string index in '[...]'")
                keys.append(str_value[1:-1])
                if self._next()[0] != "rbracket":
                    raise ResolverError("missing ']' in index expression")
            else:
                break
        if root not in self.ctx:
            self._fail_unknown(root)
        cursor = self.ctx[root]
        for key in keys:
            if isinstance(cursor, dict) and key in cursor:
                cursor = cursor[key]
            else:
                self._fail_unknown("%s.%s" % (root, key))
        return cursor

    def _fail_unknown(self, what):
        if self.lenient:
            raise _Unknown(what)
        raise ResolverError("unknown reference %r in ${{ }} expression" % what)


def _ado_eq(left, right):
    if isinstance(left, bool) or isinstance(right, bool):
        def to_bool(value):
            if isinstance(value, bool):
                return value
            return str(value).lower() == "true"

        return to_bool(left) == to_bool(right)
    return str(left).lower() == str(right).lower()


def _apply_function(name, args):
    if name == "eq":
        return _ado_eq(args[0], args[1])
    if name == "ne":
        return not _ado_eq(args[0], args[1])
    if name == "and":
        return all(bool(arg) for arg in args)
    if name == "or":
        return any(bool(arg) for arg in args)
    if name == "not":
        return not bool(args[0])
    if name == "always":
        return True
    if name == "succeeded":
        return True
    if name == "failed":
        return False
    raise ResolverError("unsupported function %s() in expression" % name)


def _eval_expr(expr, ctx, lenient):
    tokens = _tokenize(expr)
    return _ExprParser(tokens, ctx, lenient).parse()


# ===========================================================================
# Compile-time ``${{ }}`` layer
# ===========================================================================
_CONDITIONAL_RE = re.compile(r"^\$\{\{\s*(if|elseif|else)\b\s*(.*?)\s*\}\}$", re.DOTALL)
_TEMPLATE_RE = re.compile(r"\$\{\{(.*?)\}\}")


def _match_conditional(key):
    if not isinstance(key, str):
        return None
    match = _CONDITIONAL_RE.match(key)
    if not match:
        return None
    return match.group(1), match.group(2)


def _single_conditional_item(item):
    """If ``item`` is a one-key mapping whose key is a conditional, return it."""
    if isinstance(item, dict) and len(item) == 1:
        (key, value), = item.items()
        cond = _match_conditional(key)
        if cond:
            return cond[0], cond[1], value
    return None


def _to_str(value):
    if isinstance(value, bool):
        return "true" if value else "false"
    if value is None:
        return ""
    return str(value)


def _subst_template_str(text, ctx):
    def repl(match):
        return _to_str(_eval_expr(match.group(1).strip(), ctx, lenient=False))

    return _TEMPLATE_RE.sub(repl, text)


def _branch_decision(kind, expr, ctx, branch_taken):
    """Resolve one if/elseif/else key; return (take, new_branch_taken)."""
    if kind == "if":
        take = _eval_bool(expr, ctx)
        return take, take
    if kind == "elseif":
        if branch_taken:
            return False, True
        take = _eval_bool(expr, ctx)
        return take, take
    # else
    return (not branch_taken), True


def _eval_bool(expr, ctx):
    value = _eval_expr(expr, ctx, lenient=False)
    return bool(value)


def eval_compile(node, ctx):
    """Resolve ``${{ }}`` conditional insertion + inline substitution in ``node``.

    Does not expand templates or substitute ``$()`` macros.
    """
    if isinstance(node, dict):
        out = {}
        branch_taken = None
        for key, value in node.items():
            cond = _match_conditional(key)
            if cond:
                take, branch_taken = _branch_decision(cond[0], cond[1], ctx, branch_taken)
                if take:
                    merged = eval_compile(value, ctx)
                    if not isinstance(merged, dict):
                        raise ResolverError(
                            "mapping-level ${{ %s }} insertion must contain a mapping" % cond[0]
                        )
                    out.update(merged)
            else:
                branch_taken = None
                out[key] = eval_compile(value, ctx)
        return out
    if isinstance(node, list):
        out = []
        branch_taken = None
        for item in node:
            cond = _single_conditional_item(item)
            if cond:
                take, branch_taken = _branch_decision(cond[0], cond[1], ctx, branch_taken)
                if take:
                    block = eval_compile(cond[2], ctx)
                    if isinstance(block, list):
                        out.extend(block)
                    else:
                        out.append(block)
            else:
                branch_taken = None
                out.append(eval_compile(item, ctx))
        return out
    if isinstance(node, str):
        return _subst_template_str(node, ctx)
    return node


# ===========================================================================
# ``$()`` runtime macro layer (ADO-faithful, DD7)
# ===========================================================================
_MACRO_RE = re.compile(r"\$\(([^()]*)\)")

MAX_SUBST_ITERS = 20


def substitute_macros(text, known):
    """Substitute only ``$(Name)`` tokens whose ``Name`` is a known variable.

    Innermost-first; every other ``$(...)`` is preserved verbatim. Known values
    that themselves contain macros are resolved recursively. A cycle guard
    bounds the recursion so a self-referential override cannot loop forever.
    """
    for _ in range(MAX_SUBST_ITERS):
        replaced = False

        def repl(match):
            nonlocal replaced
            name = match.group(1).strip()
            if name in known:
                replaced = True
                return _to_str(known[name])
            return match.group(0)

        new_text = _MACRO_RE.sub(repl, text)
        if not replaced:
            return new_text
        text = new_text
    raise ResolverError(
        "macro substitution did not converge in %d iterations" % MAX_SUBST_ITERS
    )


def _subst_macros_deep(obj, known):
    if isinstance(obj, str):
        return substitute_macros(obj, known)
    if isinstance(obj, dict):
        return {key: _subst_macros_deep(value, known) for key, value in obj.items()}
    if isinstance(obj, list):
        return [_subst_macros_deep(value, known) for value in obj]
    return obj


# ===========================================================================
# ``condition:`` layer (FR7)
# ===========================================================================
def eval_condition(expr, variables, notes=None):
    """Evaluate a ``condition:`` expression. Never fails-fast.

    ``always()`` / ``succeeded()`` / ``failed()`` and known-operand expressions
    are evaluated against ``variables``; anything unknown or unparseable defaults
    to ``True`` with a logged note.
    """
    if expr is None:
        return True
    ctx = {"parameters": {}, "variables": variables}
    try:
        return bool(_eval_expr(str(expr), ctx, lenient=True))
    except _Unknown as unknown:
        if notes is not None:
            notes.append("condition %r has unknown operand %s; defaulting to true" % (expr, unknown))
        return True
    except ResolverError as error:
        if notes is not None:
            notes.append("condition %r could not be evaluated (%s); defaulting to true" % (expr, error))
        return True


# ===========================================================================
# Variable resolution
# ===========================================================================
def _resolve_variables(raw, base_vars):
    """Resolve a ``variables:`` block (list-of-name/value or mapping) to a dict."""
    if raw is None:
        return {}
    ctx = {"parameters": {}, "variables": base_vars}
    resolved = eval_compile(raw, ctx)
    out = {}
    if isinstance(resolved, list):
        for entry in resolved:
            if isinstance(entry, dict) and "name" in entry:
                out[entry["name"]] = entry.get("value")
    elif isinstance(resolved, dict):
        for key, value in resolved.items():
            out[key] = value
    return out


# ===========================================================================
# Step / template resolution
# ===========================================================================
def _detect_kind(step, file, line):
    present = [key for key in _STEP_KIND_KEYS if key in step]
    if not present:
        raise ResolverError(
            "unsupported step (no known step kind in keys %s) at %s:%s"
            % (sorted(step.keys()), file, line)
        )
    if len(present) > 1:
        raise ResolverError(
            "ambiguous step: multiple step-kind keys %s at %s:%s"
            % (present, file, line)
        )
    return present[0]


def _build_step(raw_item, ctx, known):
    file = getattr(raw_item, "__file__", None)
    line = getattr(raw_item, "__line__", None)
    resolved = eval_compile(raw_item, ctx)
    kind = _detect_kind(resolved, file, line)

    notes = []
    condition = resolved.get("condition")
    step = Step(
        kind=kind,
        display_name=resolved.get("displayName"),
        working_directory=substitute_macros(resolved["workingDirectory"], known)
        if isinstance(resolved.get("workingDirectory"), str)
        else resolved.get("workingDirectory"),
        env=_subst_macros_deep(resolved.get("env") or {}, known),
        condition=condition,
        condition_result=eval_condition(condition, known, notes),
        file=file,
        line=line,
    )
    if step.display_name is not None:
        step.display_name = substitute_macros(_to_str(step.display_name), known)

    if kind in ("script", "bash"):
        step.body = substitute_macros(_to_str(resolved[kind]), known)
    elif kind == "checkout":
        step.repo = resolved["checkout"]
    elif kind == "publish":
        step.body = substitute_macros(_to_str(resolved["publish"]), known)
        step.artifact = resolved.get("artifact")
    elif kind == "download":
        step.source = resolved["download"]
        step.artifact = resolved.get("artifact")
    elif kind == "task":
        task_name = resolved["task"]
        if task_name not in KNOWN_TASKS:
            raise ResolverError(
                "unsupported task %r at %s:%s" % (task_name, file, line)
            )
        step.task = task_name
        step.inputs = _subst_macros_deep(resolved.get("inputs") or {}, known)
    return step


def _resolve_template(item, ctx, base_dir, known, out):
    template_rel = item["template"]
    if isinstance(template_rel, str):
        template_rel = _subst_template_str(template_rel, ctx)
    template_path = os.path.normpath(os.path.join(base_dir, template_rel))
    template = load_yaml(template_path)

    defaults = {}
    for param in template.get("parameters", []) or []:
        defaults[param["name"]] = param.get("default")
    passed = eval_compile(item.get("parameters", {}) or {}, ctx)
    params = dict(defaults)
    params.update(passed)

    new_ctx = {"parameters": params, "variables": ctx["variables"]}
    _resolve_steps_list(
        template.get("steps", []) or [], new_ctx, os.path.dirname(template_path), known, out
    )


def _resolve_steps_list(raw_list, ctx, base_dir, known, out):
    branch_taken = None
    for item in raw_list:
        cond = _single_conditional_item(item)
        if cond:
            take, branch_taken = _branch_decision(cond[0], cond[1], ctx, branch_taken)
            if take:
                block = cond[2] if isinstance(cond[2], list) else [cond[2]]
                _resolve_steps_list(block, ctx, base_dir, known, out)
            continue
        branch_taken = None
        if not isinstance(item, dict):
            raise ResolverError("unsupported step entry %r" % (item,))
        if "template" in item:
            _resolve_template(item, ctx, base_dir, known, out)
        else:
            out.append(_build_step(item, ctx, known))
    return out


# ===========================================================================
# Pipeline-level helpers
# ===========================================================================
def find_job(pipeline, job_name):
    """Return (stage, job) for ``job_name`` or raise ResolverError."""
    for stage in pipeline.get("stages", []) or []:
        for job in stage.get("jobs", []) or []:
            if job.get("job") == job_name:
                return stage, job
    raise ResolverError("job %r not found in pipeline" % job_name)


def list_jobs(pipeline):
    """Return an ordered list of stage/job descriptors for ``--list``."""
    stages = []
    for stage in pipeline.get("stages", []) or []:
        jobs = []
        for job in stage.get("jobs", []) or []:
            jobs.append(
                {
                    "job": job.get("job"),
                    "displayName": job.get("displayName"),
                    "condition": job.get("condition"),
                }
            )
        stages.append(
            {
                "stage": stage.get("stage"),
                "displayName": stage.get("displayName"),
                "jobs": jobs,
            }
        )
    return stages


def build_known_vars(pipeline, job, cli_vars=None):
    """Build the macro lookup table for ``$()`` substitution for one job.

    Precedence (low -> high): pseudo-vars < pipeline variables < job variables
    < CLI ``--var`` overrides.
    """
    cli_vars = cli_vars or {}
    pseudo = dict(DEFAULT_PSEUDO_VARS)

    base_for_eval = dict(pseudo)
    base_for_eval.update(cli_vars)
    pipeline_vars = _resolve_variables(pipeline.get("variables"), base_for_eval)

    base_for_eval.update(pipeline_vars)
    job_vars = _resolve_variables(job.get("variables"), base_for_eval)

    known = dict(pseudo)
    known.update(pipeline_vars)
    known.update(job_vars)
    known.update(cli_vars)
    return known


def resolve_job(pipeline_path, job_name, cli_vars=None):
    """Resolve a job into a flat ``list[Step]``.

    Returns ``(steps, job_condition_result)``.
    """
    pipeline = load_yaml(pipeline_path)
    _, job = find_job(pipeline, job_name)
    known = build_known_vars(pipeline, job, cli_vars)

    ctx = {"parameters": {}, "variables": known}
    base_dir = os.path.dirname(os.path.abspath(str(pipeline_path)))

    steps = []
    _resolve_steps_list(job.get("steps", []) or [], ctx, base_dir, known, steps)
    job_condition_result = eval_condition(job.get("condition"), known)
    return steps, job_condition_result
