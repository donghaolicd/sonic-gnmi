"""Read-only YAML loader for ADO pipeline / template files.

The loader returns the raw AST (nested dict/list/scalars) exactly as written
in the YAML, preserving mapping key order and attaching source location
(file + line number) to every mapping and sequence so the resolver can emit
precise "file + line" error messages (NFR3).

ADO ``${{ ... }}`` expressions are syntactically ordinary YAML scalars, so they
survive loading as raw strings without any special handling -- they are *not*
evaluated here. All resolution happens later in ``resolver.py``.

This module never writes to disk; the canonical YAML stays untouched.
"""

import yaml


class LineDict(dict):
    """A dict that records the source file and line where it was defined."""


class LineList(list):
    """A list that records the source file and line where it was defined."""


class _LineLoader(yaml.SafeLoader):
    """SafeLoader subclass that preserves key order and source locations."""


def _construct_mapping(loader, node):
    mapping = LineDict()
    mapping.__line__ = node.start_mark.line + 1
    mapping.__file__ = getattr(loader, "_ado_filename", None)
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=True)
        value = loader.construct_object(value_node, deep=True)
        mapping[key] = value
    return mapping


def _construct_sequence(loader, node):
    seq = LineList()
    seq.__line__ = node.start_mark.line + 1
    seq.__file__ = getattr(loader, "_ado_filename", None)
    for child_node in node.value:
        seq.append(loader.construct_object(child_node, deep=True))
    return seq


_LineLoader.add_constructor("tag:yaml.org,2002:map", _construct_mapping)
_LineLoader.add_constructor("tag:yaml.org,2002:seq", _construct_sequence)


def load_yaml(path):
    """Load ``path`` (read-only) into a raw AST with source locations.

    Returns the top-level node (normally a :class:`LineDict`). Mapping and
    sequence nodes carry ``__file__`` / ``__line__`` attributes.
    """
    path = str(path)
    with open(path, "r") as handle:
        text = handle.read()
    loader = _LineLoader(text)
    loader._ado_filename = path
    try:
        return loader.get_single_data()
    finally:
        loader.dispose()
