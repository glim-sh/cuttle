---
type: Decision
title: Driver output is masked inside the container, where the values live
description: cuttle pw streams the bundled driver's output through the daemon's /mask route from inside the container, so secret values never reach the host; vendor-prefixed tokens are captured under TOKEN_n rather than destroyed.
tags: [secrets, masking, playwright-cli, drivers]
status: stable
generated: { by: claude-code/claude-opus-5, at: "2026-09-18T18:20:00+00:00" }
sources:
  - id: code
    resource: /packages/cuttle/internal/cli/maskexec.go
    title: The in-container wrapper (cuttle __mask-exec) and the host's probe in playwright.go
  - id: route
    resource: /packages/cuttle/internal/serve/masking.go
    title: handleMask and maskOutput
  - id: plan
    resource: /docs/plans/2608-26-daemon-owned-secrets.md
    title: Daemon-owned secrets plan, sections 7.1, 7.4 and 8.7
  - id: maintainer
    resource: "maintainer direction during the output-masking work, 2026-09-18"
    title: Maintainer decision
---

# Decision

The bundled driver's stdout and stderr are masked by the daemon, not by the
host CLI: `cuttle pw` execs `cuttle __mask-exec` beside the daemon, and that
wrapper streams line batches to the loopback-only `POST /mask` route.[^code][^route]

Why there: masking needs the values to compare against, and the daemon is
the only place that holds them. Masking on the host would mean shipping
every secret to the host process - the leak daemon-owned secrets exist to
prevent.[^plan]

What it covers, deliberately narrow:[^maintainer]

- Exact values the store holds, in their common encodings, under the same
  length floors as the log masker. This is the boundary playwright-mcp's
  `redactSecrets` (exact configured values) and browser-use's sensitive-data
  handling draw.[^plan]
- Vendor-prefixed tokens only (one slice in `internal/mask/mask.go`), no
  entropy rule, nothing personal. A match is KEPT in the store as `TOKEN_n`
  and fillable as `{{cuttle:TOKEN_n}}`: a one-time token the page showed once
  must not be traded from a leak into a loss.

Consequences: a secret cuttle never held, or one the page reformats or
splits, still passes through; a driver attached from the host gets no
masking. Masking is a safety net - capturing a one-time credential before
looking at the page remains the rule. `capture --to file:/exec:` also keeps
the value in the store, since holding it is what masks the next snapshot.

Related: [The aria snapshot renders field values](/findings/aria-snapshot-renders-secret-values.md).

[^code]: The in-container wrapper (cuttle __mask-exec) and the host's probe in playwright.go
[^route]: handleMask and maskOutput
[^plan]: Daemon-owned secrets plan, sections 7.1, 7.4 and 8.7
[^maintainer]: Maintainer decision
