#!/usr/bin/env python3
"""Fail if SKILL.md's frontmatter version drifts from the package version.

The `cuttle up` briefing prints the installed cuttle-browser version so an
agent holding a stale installed copy of the skill can notice and rerun
`cuttle skill`. That detection only works if SKILL.md's metadata.version is
kept in lockstep with [project].version - this check enforces it.
"""

from __future__ import annotations

import re
import sys
import tomllib
from pathlib import Path

root = Path(__file__).resolve().parent.parent
pkg = tomllib.loads((root / "pyproject.toml").read_text())["project"]["version"]
m = re.search(r'^\s*version:\s*"([^"]+)"', (root / "SKILL.md").read_text(), re.M)
skill = m.group(1) if m else None
if skill != pkg:
    sys.exit(
        f"SKILL.md metadata.version {skill!r} != pyproject [project].version {pkg!r}"
        " - bump them together (the briefing's stale-skill detection depends on it)"
    )
print(f"skill version in lockstep ({pkg})")
