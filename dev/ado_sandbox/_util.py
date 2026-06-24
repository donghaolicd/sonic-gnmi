"""Shared helpers for the ado-sandbox executor and task stubs."""


def log(stream, message):
    """Write a single ``[ado-sandbox]``-prefixed line to ``stream``."""
    stream.write("[ado-sandbox] %s\n" % message)
