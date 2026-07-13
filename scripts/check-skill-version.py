#!/usr/bin/env python3
"""Check (or with --fix, set) SKILL.md's frontmatter version against the package.

The `cuttle up` briefing prints the installed cuttle-browser version so an
agent holding a stale installed copy of the skill can notice and rerun
`cuttle skill`. That detection only works if SKILL.md's metadata.version is
kept in lockstep with [project].version - this check enforces it, and
`just release` runs it with --fix to do the bump.
"""

from __future__ import annotations

import re
import sys
import tomllib
from pathlib import Path

VERSION_RE = re.compile(r'^(\s*version:\s*)"([^"]+)"', re.M)

root = Path(__file__).resolve().parent.parent
skill_md = root / "SKILL.md"
pkg = tomllib.loads((root / "pyproject.toml").read_text())["project"]["version"]

if "--fix" in sys.argv:
    text, n = VERSION_RE.subn(rf'\g<1>"{pkg}"', skill_md.read_text(), count=1)
    if not n:
        sys.exit("SKILL.md has no metadata.version line to set")
    skill_md.write_text(text)

m = VERSION_RE.search(skill_md.read_text())
skill = m.group(2) if m else None
if skill != pkg:
    sys.exit(
        f"SKILL.md metadata.version {skill!r} != pyproject [project].version {pkg!r}"
        " - bump them together (the briefing's stale-skill detection depends on it)"
    )
print(f"skill version in lockstep ({pkg})")
