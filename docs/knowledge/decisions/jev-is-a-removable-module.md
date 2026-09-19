---
type: Decision
title: jev is a removable module
description: Only internal/cli/jevbrowse.go (and its test) may import internal/jev, enforced by depguard, so the experimental jev-browse loop can be cut out at any time; cli code that needs aria parsing keeps its own small parser.
tags: [jev-browse, architecture, lint]
status: stable
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T11:30:00+01:00" }
sources:
  - id: maintainer
    resource: "maintainer decision, 2026-09-19: jev is experimental and must be removable at any time"
    title: Maintainer decision
  - id: lint
    resource: /packages/cuttle/.golangci.yml
    title: depguard rule jev-only-behind-jev-browse
  - id: pr
    resource: https://github.com/glim-sh/cuttle/pull/99
    title: "fix(cli): name the in-page dialog behind a pw click timeout (added the rule)"
---

# Decision

`internal/jev` is imported only by `internal/cli/jevbrowse.go` and its
test. A depguard rule in `packages/cuttle/.golangci.yml`
(`jev-only-behind-jev-browse`) fails `just check` on any other
import.[^lint][^pr]

Why: jev-browse is experimental. Removing it must stay a matter of deleting
`internal/jev`, `jevbrowse.go` and its docs, with nothing else in the cli
depending on it.[^maintainer]

Consequence: cli code that needs aria-snapshot parsing - `cuttle pw`'s
dialog hint in `playwright.go` is the first case - keeps its own small
parser instead of importing jev's. The duplication is the price of
removability and is accepted.[^maintainer][^pr]

[^maintainer]: Maintainer decision
[^lint]: depguard rule jev-only-behind-jev-browse
[^pr]: fix(cli): name the in-page dialog behind a pw click timeout (added the rule)
