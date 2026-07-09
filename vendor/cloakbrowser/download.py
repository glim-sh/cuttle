"""Binary resolution for cuttle.

cuttle always runs against a baked local Chromium fork, selected via the
CLOAKBROWSER_BINARY_PATH env var. This module keeps only the local-override
path; the upstream download and binary-verification machinery is not vendored.
"""

from __future__ import annotations

import logging
from pathlib import Path

from .config import get_local_binary_override

logger = logging.getLogger("cloakbrowser")


def ensure_binary(browser_version: str | None = None) -> str:
    """Return the local Chromium binary path from CLOAKBROWSER_BINARY_PATH.

    cuttle always sets CLOAKBROWSER_BINARY_PATH to a baked fork binary. This
    resolves it, erroring clearly if it is unset or points at a missing file.
    """
    local_override = get_local_binary_override()
    if not local_override:
        raise RuntimeError(
            "CLOAKBROWSER_BINARY_PATH is not set. cuttle ships no binary download; "
            "point it at a local stealth Chromium build."
        )
    path = Path(local_override)
    if not path.exists():
        raise FileNotFoundError(
            f"CLOAKBROWSER_BINARY_PATH set to '{local_override}' but file does not exist"
        )
    logger.info("Using local binary override: %s", local_override)
    return str(path)
