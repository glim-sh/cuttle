---
type: Finding
title: Orchestrator reasoning effort is the largest browse-time lever
description: On a humanized multi-page flow driven through cuttle pw by a headless claude -p orchestrator, reasoning effort moved wall time more than model choice or context size; most of a run is tool execution and turn overhead, not LLM time. On merged main, pw at low effort (~100-130s) is the fastest reliable configuration.
tags: [benchmark, performance, cuttle-pw, orchestrator, humanize]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T14:05:00+01:00" }
sources:
  - id: bench
    resource: "benchmark harness runs on 2026-09-18/19 against a signed-in site: one fixed flow, humanize on, headless claude -p orchestrator with a fresh context per run, plain cuttle pw (session record, no durable link)"
    title: Browse benchmark runs
  - id: main
    resource: "benchmark harness rerun on 2026-09-19 12:29-13:05 UTC+1 on merged main at 9ca9386 (#100 + #102 + #103), same flow and orchestrator (claude -p, Opus), first with the container still on the pre-#103 image, then on an image built from main (session record, no durable link)"
    title: Merged-main benchmark rerun
---

# Finding

The flow: a filtered search -> open 3 results -> drill into a related page
-> open 3 linked entries, about 12 clicks on a signed-in site, humanized input
on, driven by a headless `claude -p` orchestrator with a fresh context,
through plain `cuttle pw`.[^bench]

| Arm | Wall time | Turns | Cost |
|---|---|---|---|
| effort low | 101s, 103s | 12 | ~$1.00 |
| effort high (default), 4 runs | 158-187s | 20-29 | $1.9-4.0 |
| effort max | 162s, 235s | 26-32 | - |
| Haiku 4.5 | 140s, 301s (unstable) | - | - |
| Sonnet 5 | 207s | - | - |

What the numbers say:[^bench]

- **Effort is the lever.** Low effort did the same flow in about 60% of
  the high-effort time at a half to a quarter of the cost, mostly by taking
  fewer turns. Max effort bought nothing over high.
- **LLM time is the minority of a run.** At high effort, API time was
  50-66s of a ~170s run; the rest was tool execution and CLI turn overhead.
- **A large prepended context did not slow turns.** A 140k-token context
  ran ~2.9s per turn against ~2.5s without it. That arm is confounded - the
  context contained the route - so it says nothing about whether context
  helps, only that it did not hurt latency.
- **Humanized input sets the floor.** Each humanized click costs ~3.5s
  including the page's reaction, which puts this flow at a 60-80s floor
  whatever the orchestrator does. That floor stays; see
  [Humanized input is the value proposition](/decisions/humanize-over-speed.md).

## On merged main

The rerun on main after #100 (one docker exec per pw verb), #102 (jev-browse
speed) and #103 (host-file snapshots) landed, same flow and orchestrator,
Opus:[^main]

| Arm | Wall time | Turns | Cost |
|---|---|---|---|
| pw, effort low, 4 runs | 96, 129, 116, 104s | 12-17 | $1.0-2.4 |
| pw, effort high, 3 runs | 148, 161, 153s | 17-20 | $1.5-1.7 |

A fourth high-effort run (68s, 11 turns) was a harness failure - the
orchestrator returned a non-JSON answer - not a browse failure, and is
excluded. The first two runs of each arm used the merged-main CLI against a
container still on the pre-#103 image, the rest a container rebuilt from
main, so #103's host snapshot was exercised only in the latter; it did not
measurably move pw timings. Against the pre-merge 158-187s at high effort,
main's pw numbers are in the same band, and pw at low effort remains the
fastest reliable configuration at ~100-130s.[^main] The same rerun's
jev-browse arms are in
[jev-browse slowness was the driver path](/findings/jev-browse-slowness-was-the-driver-path.md).

One consequence for the parked serve-hosted client experiment: its
~250ms-per-call saving is worth roughly 5-8s of a ~100s run, for ~370 lines
tied to playwright-cli internals - see
[cost of one cuttle pw call](/findings/pw-call-cost-breakdown.md).

Every arm of this rerun hit the same site-side overlay on one step; the pw
arms recovered with a snapshot or reload, and only low-effort jev-browse
did not - see
[a client-side app transition can update URL and title while the body stays hidden](/findings/spa-transition-hides-body-behind-overlay.md).

[^bench]: Browse benchmark runs
[^main]: Merged-main benchmark rerun
