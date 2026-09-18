---
type: Decision
title: playwright-cli is the driver interface for higher-level automation
description: cuttle composes the bundled playwright-cli for all higher-level browsing automation (cuttle pw, cuttle jev-browse) - never raw CDP, never its own snapshot or ref semantics.
tags: [drivers, playwright-cli, cuttle-pw, jev-browse, stealth]
status: stable
generated: { by: claude-code/claude-opus-5, at: "2026-09-18T15:55:00+00:00" }
sources:
  - id: maintainer
    resource: "maintainer decision during the jev-browse design discussion, 2026-09-18"
    title: Maintainer decision
  - id: survey
    resource: "session research, 2026-09-18: ecosystem survey of 12 open-source agent-browser repos in the jev-browse style (no durable link)"
    title: Ecosystem survey
  - id: code
    resource: /packages/cuttle/internal/cli/playwright.go
    title: cuttle pw wrapper
---

# Decision

Every higher-level browsing feature cuttle ships - the `cuttle pw`
passthrough and `cuttle jev-browse` alike - is built by composing the
bundled playwright-cli. cuttle does not drive pages over raw CDP itself and
does not reimplement snapshot, ref or actionability semantics.[^maintainer][^code]

Why:

- **Microsoft maintains the hard parts.** The aria snapshot, element refs
  and actionability waiting are the pieces that break on real pages; the
  driver owns them and fixes them upstream.[^maintainer]
- **The version cannot drift silently.** The driver is pinned three ways -
  `versions.env`, the Dockerfile ARG and a Go const - and a test fails when
  they disagree, so any behavior cuttle leans on changes only through a
  reviewed pin bump.[^code]
- **Stealth guarantees hold either way.** The driver attaches through
  cuttle's CDP mux, so humanized input and `{{cuttle:NAME}}` secret
  sentinels apply to its verbs exactly as to any other client.[^maintainer]
- **The exec cost is acceptable.** Each verb is a separate process call,
  roughly 200-600ms. Speed of action is explicitly not the product's goal -
  see [Humanized input is the value proposition](/decisions/humanize-over-speed.md) -
  so this overhead buys nothing worth reimplementing the driver for.[^maintainer]

A detection argument reinforces it: a survey of 12 agent-browser repos in
the jev-browse style found every one hand-rolling page perception with
DOM-stamped attributes (ids or data attributes written into the page to
address elements) - a beacon any page script can read.[^survey] Delegating
perception to the driver, whose scripts run in an isolated world, avoids
that for free.

Consequence: a new feature that needs to read or act on a page goes through
the driver. A gap in the driver is a reason to work around it narrowly
(and file upstream), not to start a parallel CDP perception layer.

Related: [playwright-cli attach and session model](/findings/playwright-cli-attach-model.md).

[^maintainer]: Maintainer decision
[^survey]: Ecosystem survey
[^code]: cuttle pw wrapper
