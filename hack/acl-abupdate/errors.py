"""The exception the harness and its modules raise when a step can't go on.

It's a module of its own so that flexcluster.py, flexinstall.py, and omaha.py can raise it without
importing abupdate.py, which runs as __main__.
"""


class StepError(Exception):
    """A step can't go on: the VM or the cluster isn't in the state the step needs, or a command
    it ran failed. A scenario that raises it fails; a command that does exits 1."""
