---
type: Finding
title: Orchestrator reasoning effort is the largest browse-time lever
description: On a humanized multi-page flow driven through cuttle pw by a headless claude -p orchestrator, reasoning effort moved wall time more than model choice or context size; most of a run is tool execution and turn overhead, not LLM time.
tags: [benchmark, performance, cuttle-pw, orchestrator, humanize]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T11:40:00+01:00" }
sources:
  - id: bench
    resource: "benchmark harness runs on 2026-09-18/19 against a signed-in job site: one fixed flow, humanize on, headless claude -p orchestrator with a fresh context per run, plain cuttle pw (session record, no durable link)"
    title: Browse benchmark runs
---

# Finding

The flow: feed -> Jobs -> a job search -> open 3 postings -> a company page
-> its People tab -> open 3 people, on a signed-in job site, humanized input
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

Related: [jev-browse slowness was the driver path](/findings/jev-browse-slowness-was-the-driver-path.md),
[cost of one cuttle pw call](/findings/pw-call-cost-breakdown.md).

[^bench]: Browse benchmark runs
