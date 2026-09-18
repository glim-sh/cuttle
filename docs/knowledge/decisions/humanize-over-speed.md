---
type: Decision
title: Humanized input is the value proposition
description: Human-paced input is core to cuttle's stealth promise; decision-loop optimizations must never trade it away for action speed.
tags: [stealth, humanize, drivers]
status: stable
generated: { by: claude-code/fable-5, at: "2026-09-18T14:30:00+00:00" }
sources:
  - id: maintainer
    resource: "maintainer direction during the playwright-cli bundling work, 2026-09-18"
    title: Maintainer decision
---

# Decision

Humanized, human-paced input (clicks, typing, motion jitter) stays on in
every documented flow. Losing wall-clock time to it is a feature, not a
bug.[^maintainer]

Fast decision loops (System-One models such as TypeSafe Jev picking the next
element in ~100-300ms) make per-step decision latency nearly free, which
creates pressure to also strip the ~0.5s humanized action to showcase speed.
Resist it: cuttle's promise is "websites do not block it", and human-paced
action is part of how sessions stay unremarkable. A benchmark or example
that disables humanization advertises a configuration we do not stand
behind.

Consequence for docs and tooling: frame loop speedups as saving decision
latency, never action latency, and do not surface a "disable humanize for
speed" knob in READMEs, examples, or benchmark instructions.

[^maintainer]: Maintainer decision
