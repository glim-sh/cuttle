---
okf_version: "0.2"
title: cuttle knowledge bundle
description: Durable decisions and findings behind cuttle's design, with rationale and evidence.
---

# cuttle knowledge bundle

Durable project knowledge: decisions with rationale, findings with evidence.
A concept belongs here only if it is true and useful in a fresh clone. State
(versions in flight, work status, counts) stays in git, PRs and the release
machinery; secrets never enter the repo; standing orders live in AGENTS.md
with at most a one-line pointer here, while the rationale lives in the
concept. This is a PUBLIC repository: no internal infrastructure references,
no named commercial targets, no credentials - the same non-negotiables as
AGENTS.md.

Type vocabulary: `Decision`, `Finding`, `Reference`, `Runbook`. Layout:
`decisions/`, `findings/` (add `references/`, `runbooks/` on first need).
This bundle is hand-indexed: when adding or changing a concept, update the
lists below (title, link, the concept's `description` verbatim).

## Decisions

- [Humanized input is the value proposition](decisions/humanize-over-speed.md) - Human-paced input is core to cuttle's stealth promise; decision-loop optimizations must never trade it away for action speed.

## Findings

- [playwright-cli attach and session model](findings/playwright-cli-attach-model.md) - How the bundled playwright-cli 0.1.20 decides to attach vs launch, where its session daemon keeps state, and the failure wordings the cuttle pw wrapper relies on.
- [Downloads API is seed-keyed; the reserved seed is unreachable in pool mode](findings/downloads-seed-keying.md) - GET /downloads resolves through a seed and rejects the reserved __default__ seed in pool mode, so driver-written files there must be asserted via exec, not the API.
- [The profile dir is the session's artifact store, not Chrome's scratch](findings/profile-dir-is-artifact-store.md) - A relaunch of the same seed must reuse its profile dir - downloads and driver outputs live there; only an ephemeral run may delete it.
