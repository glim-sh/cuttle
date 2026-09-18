---
type: Decision
title: One driver at a time, enforced by a lease in the daemon
description: Exclusive driving of a browser is a TTL lease held in cuttle serve, keyed by seed, with explicit takeover - not a host file lock and not a second browser.
tags: [serve, drivers, concurrency, jev-browse, cuttle-pw]
status: stable
generated: { by: claude-code/claude-opus-5, at: "2026-09-18T16:52:49+00:00" }
sources:
  - id: maintainer
    resource: "maintainer decision during the session-lease design discussion, 2026-09-18"
    title: Maintainer decision
  - id: lease
    resource: /packages/cuttle/internal/serve/lease.go
    title: Daemon lease table and HTTP routes
  - id: client
    resource: /packages/cuttle/internal/cli/lease.go
    title: CLI lease client, heartbeat and cuttle pw gate
  - id: ops
    resource: /docs/OPERATING.md
    title: "OPERATING.md, One driver at a time"
---

# Decision

Exclusive driving of a browser is a lease held in memory by `cuttle serve`,
keyed by seed (in session mode, the one default seed): an owner label, a random
token and an expiry, 120 seconds without a renew. `cuttle jev-browse` holds it
for its whole run with a heartbeat at a third of the TTL; `cuttle pw` refuses
verbs that drive the page while someone else holds it and lets read verbs
through; both take the browser over only on an explicit `--takeover`.[^maintainer][^lease][^client]

# Why concurrency control, not isolation

A browser cannot run one profile twice - the profile directory is locked to the
process using it - and the value of the session is exactly that profile: its
logins, its cookies, the page a person was looking at. A second driver cannot
get a private copy of it, so the only honest option is to take turns on the one
browser. Without that, two drivers interleave clicks, typing and navigations on
the same page and both fail in ways that look like site flakiness.[^maintainer]

# Why in the daemon

- **One clock.** Expiry is judged by one process's monotonic clock. A lock file
  shared between hosts would compare timestamps from clocks that disagree.
- **Every backend.** The daemon runs beside the browser whether it is local
  docker, docker over ssh or a k8s pod; a host file lock would only coordinate
  drivers on the same host, and the drivers here often are not.
- **It dies with what it guards.** The lease lives in the process that owns the
  browsers, so a daemon restart clears both together; there is no stale lock
  file to clean up and nothing to persist.[^lease]

# Shape, and what was left out

- Lazy expiry on access, no background goroutine. A holder that dies frees the
  browser after one TTL without anyone stepping in.
- A forced release leaves the taker's label in the slot, so the evicted holder's
  next renew fails naming who took over instead of quietly re-granting.
- Unknown `cuttle pw` verbs count as driving (default-deny), so a new driver
  verb cannot slip past the gate.
- The CLI talks to the lease with curl inside the container, over the same exec
  the driver uses, so the gate needs no published port or tunnel.[^client]
- Plain CDP clients are not gated; the lease coordinates cuttle's own drivers.
  No config knobs beyond the TTL constant and the takeover flag.[^ops]

[^maintainer]: Maintainer decision
[^lease]: Daemon lease table and HTTP routes
[^client]: CLI lease client, heartbeat and cuttle pw gate
[^ops]: OPERATING.md, One driver at a time
