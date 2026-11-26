"""Launch Hermes gateway with trusted per-turn vehicle MCP binding enabled.

Usage:
    HERMES_HOME=/path/to/hermes-home \
    PYTHONPATH=/path/to/hermes-agent:/path/to/app_personal_agent \
    /path/to/hermes-venv/bin/python -m hermes.gateway_launcher
"""

from __future__ import annotations

import os
import sys
from pathlib import Path


def _ensure_paths() -> None:
    repo_root = str(Path(__file__).resolve().parent.parent)
    if repo_root not in sys.path:
        sys.path.insert(0, repo_root)
    hermes_repo = os.environ.get("HERMES_AGENT_DIR")
    if hermes_repo and hermes_repo not in sys.path:
        sys.path.insert(0, hermes_repo)


def main() -> None:
    _ensure_paths()
    from hermes.turn_binding_adapter import install

    install()
    from gateway.run import main as hermes_gateway_main

    hermes_gateway_main()


if __name__ == "__main__":
    main()
