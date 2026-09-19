---
type: Finding
title: Cost of one cuttle pw call
description: A cuttle pw verb pays a flat ~250-320ms for docker exec, node start and the playwright-cli bundle load before the browser does anything, while cuttle's own wrapper cost 30-80ms until it shrank to one exec; a persistent in-container client removes most of the fixed cost at a maintenance price.
tags: [cuttle-pw, performance, playwright-cli, docker]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T14:05:00+01:00" }
sources:
  - id: bench
    resource: "timing measurements against a local cuttle container, 2026-09-18/19: per-stage timings of single pw verbs and a 20-verb flow (session record, no durable link)"
    title: pw call timing measurements
  - id: onexec
    resource: https://github.com/glim-sh/cuttle/pull/100
    title: "perf(cli): run a pw verb in one docker exec, lease check included (merged)"
  - id: client
    resource: https://github.com/glim-sh/cuttle/pull/101
    title: "perf(cli): serve-hosted pw client (experiment, draft, parked; branch exp/pw-serve-client)"
---

# Finding

Where the time in one `cuttle pw` call goes:

| Stage | Cost |
|---|---|
| `docker exec` + node start + playwright-cli bundle load | ~250-320ms, flat per verb[^client] |
| of which one `docker exec` process | ~42ms[^onexec] |
| daemon round trip for a snapshot | ~45ms[^bench] |
| cuttle's own wrapper, before the one-exec change | 30-80ms[^onexec] |

The wrapper's share was extra docker processes: a `docker inspect` state
check (~26ms) before every verb and a lease-check `docker exec` (~42ms)
before a driving one. The one-exec change asks for state only after a failed
exec and runs the lease check inside the verb's own exec, taking a verb from
2-3 docker processes to one.[^onexec]

The bigger fixed cost is the exec plus node start, paid on every verb. A
serve-hosted persistent client, kept alive and talking to the session
daemon's socket, measured:[^client][^bench]

| Measure | Per-exec | Persistent client |
|---|---|---|
| snapshot | 321ms | 77ms |
| goto | 439ms | 122ms |
| click (humanized) | 1119ms | 823ms |
| 20-verb flow | 14.2s | 9.1s |
| container CPU, 20 snapshots | 5.4s | 0.16s |

Its costs: ~370 lines of code, reliance on playwright-cli's internal client
modules (not a public API, so every pin bump must re-check them), and a
second path that has to stay behaviorally equivalent to the exec path.[^client]
Against a humanized flow's floor the saving is a fraction of the run: on
merged main, plain pw at low effort does the reference flow in ~100-130s, so
~250ms per call is worth ~5-8s of it, and the experiment stays parked until
per-call latency is the bottleneck again - see
[orchestrator effort is the browse-time lever](/findings/orchestrator-effort-is-the-browse-time-lever.md).

Refines the rough 200-600ms per verb in
[playwright-cli is the driver interface](/decisions/playwright-cli-is-the-driver-interface.md).

[^bench]: pw call timing measurements
[^onexec]: perf(cli): run a pw verb in one docker exec, lease check included (merged)
[^client]: perf(cli): serve-hosted pw client (experiment, draft, parked; branch exp/pw-serve-client)
