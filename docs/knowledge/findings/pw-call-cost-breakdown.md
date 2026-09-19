---
type: Finding
title: Cost of one cuttle pw call
description: A cuttle pw verb costs ~350ms before the browser does anything - docker exec, node start and the playwright-cli bundle load - while cuttle's own wrapper adds 30-70ms; a persistent in-container client removes most of it at a maintenance price.
tags: [cuttle-pw, performance, playwright-cli, docker]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T11:30:00+01:00" }
sources:
  - id: bench
    resource: "timing measurements against a local cuttle container, 2026-09-18/19: per-stage timings of single pw verbs and a 20-verb flow (session record, no durable link)"
    title: pw call timing measurements
  - id: onexec
    resource: https://github.com/glim-sh/cuttle/pull/100
    title: "perf(cli): run a pw verb in one docker exec, lease check included (branch perf/pw-call-overhead)"
  - id: client
    resource: https://github.com/glim-sh/cuttle/pull/101
    title: "perf(cli): serve-hosted pw client (experiment, draft; branch exp/pw-serve-client)"
---

# Finding

Where the time in one `cuttle pw` call goes:[^bench]

| Stage | Cost |
|---|---|
| `docker exec` | ~100ms |
| node start + playwright-cli bundle load | ~200ms |
| daemon round trip for a snapshot | ~45ms |
| cuttle's own wrapper | 30-70ms |

The wrapper's share came mostly from extra docker processes; folding the
lease check and the state check into the verb's own exec takes a verb from
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
Against a humanized flow's floor the saving is a fraction of the run - see
[orchestrator effort is the browse-time lever](/findings/orchestrator-effort-is-the-browse-time-lever.md).

Refines the rough 200-600ms per verb in
[playwright-cli is the driver interface](/decisions/playwright-cli-is-the-driver-interface.md).

[^bench]: pw call timing measurements
[^onexec]: perf(cli): run a pw verb in one docker exec, lease check included (branch perf/pw-call-overhead)
[^client]: perf(cli): serve-hosted pw client (experiment, draft; branch exp/pw-serve-client)
