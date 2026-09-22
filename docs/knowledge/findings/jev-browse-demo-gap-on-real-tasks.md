---
type: Finding
title: Why decision-model browsing demos do not transfer to real tasks
description: An A/B of jev-browse vs plain cuttle pw on a real signed-in admin-console inventory task, then re-runs as intended, verified five concrete defects and generalized twelve causes for the gap between ecosystem demos (e.g. browser-use/jev-ultrafast's 7s Google Flights run) and real-account work; jev-browse is a cheap navigator for short hops, never a task closer.
tags: [jev-browse, benchmark, demo-gap, extract, reliability]
status: stable
stale_after: "2027-03-22T00:00:00+00:00"
generated: { by: claude-code/claude-fable-5, at: "2026-09-22T16:30:00+00:00" }
sources:
  - id: ab
    resource: "A/B run 2026-09-22 on a signed-in cloud provider admin console (session record, no durable link): identical inventory brief to two agents - one cuttle pw only (552s, 34 tool calls, complete), one jev-browse-first (630s, 60 calls, 37 jev runs, 12 pw fallbacks, complete) - then 14 direct re-runs as intended (short navigation goals, single-detail extracts) with per-step --json logs"
    title: Admin-console A/B and intended-use re-runs
  - id: ultrafast
    resource: https://github.com/browser-use/jev-ultrafast
    title: browser-use/jev-ultrafast (its docs/performance.md carries the measurement fine print)
  - id: code
    resource: /packages/cuttle/internal/jev/loop.go
    title: jev extract path (pageLines dedupe, per-line noul, typableRoles)
---

# Finding

The same real task - a complete billing/resource inventory of a signed-in
cloud admin console - run twice by identical agents, once on plain
`cuttle pw`, once jev-browse-first, then jev-browse re-run 14 times as
intended (one short navigation goal per run, no `--url` hints). Both agents
produced the same correct inventory; jev-browse-first was slower (630s vs
552s), costlier, and needed 12 `pw` fallbacks. As intended, jev-browse
navigated single hops in 4-9s with no orchestrator turns - and ended fully
right in only about half the runs.[^ab]

## Verified defects (new, all in `internal/jev`)

1. **`--extract` returns zero lines when asked for several details per
   item.** Each table cell is judged as its own line and any line "lacking a
   detail" is dropped; the same page yields the full list when asked for one
   detail ("one server name", "one invoice amount").[^ab][^code]
2. **`--extract` collapses repeated values.** `pageLines` dedupes by exact
   text, so a column with identical amounts loses rows silently (12 invoices
   came back as 4 amounts).[^ab][^code]
3. **Dropdowns are inoperable.** `typableRoles` classifies combobox as
   typable, so it is never offered as a click and typing cannot select an
   option; with or without a `--text` value the run exits 3.[^ab][^code]
4. **A link opening a new tab is clicked and never followed.** The loop has
   no tab handling; one run repeated the same click 7 times, leaving 7
   stray tabs, and spent its budget.[^ab]
5. **A vague task makes done meaningless.** "View the list on this page"
   ended done on an unrelated page after a redirect; the same run phrased
   as "view the list of S3 credentials" recovered and landed right.[^ab]

## Why demos do not transfer - twelve causes

Demo here means the ecosystem's headline runs, typified by jev-ultrafast's
7.1s Google Flights task.[^ultrafast]

1. **Reading is not picking.** Real tasks are mostly reading and judgment;
   a picker model only clicks, and extract is a line-matcher, not a reader.
2. **DONE is self-graded.** Demos verify with hand-written per-task code
   (jev-ultrafast's own fine print: "DONE is never independent evidence");
   live, the done score misfires both ways.
3. **Not clicking wins.** The winning `pw` agent captured the console's own
   JSON responses instead of clicking through it; demos never benchmark
   that arm.
4. **Public vs logged-in.** Demos fire raw synthetic input at public sites;
   cuttle humanizes every action by design
   ([humanize-over-speed](/decisions/humanize-over-speed.md)), a ~3s/click
   floor that alone erases headline speed.
5. **Tasks are chosen inside the action space.** Dropdowns, new tabs,
   iframes, shadow DOM, canvas are documented-unsupported in cuttle's jev
   and jev-ultrafast alike, and real UIs are full of them.
6. **Real pages lie mid-transition.** SPA overlays update URL and title
   while the body hides
   ([spa-transition](/findings/spa-transition-hides-body-behind-overlay.md)).
7. **The driver path, not the model, is the cost.** Decisions are ~200ms;
   the seconds are snapshots, spawns and dead clicks
   ([driver path](/findings/jev-browse-slowness-was-the-driver-path.md)).
8. **No recovery reflex.** A wrong pick makes an LLM re-orient; the picker
   loop repeats or wanders (defect 4).
9. **Confidence does not discriminate.** Correct and wrong picks overlap
   ([decision layer](/decisions/jev-model-judges-loop-acts.md)); no
   threshold buys reliability back.
10. **Safety eats capability on real accounts.** Write-guards refuse
    harmless submits and value-redaction blinds the model to field and
    option text - protection a public-site demo never pays for.
11. **Task phrasing is an unwritten API.** Vague goal, false done
    (defect 5); multi-detail extract, zero lines (defect 1).
12. **Thin stats.** jev-ultrafast's 25% headline is 3 matched pairs
    (p=0.25, self-disclosed) with setup and verification outside the
    clock.[^ultrafast]

## Consequence

jev-browse's viable role is unchanged from the
[stress-round verdict](/findings/stress-round-verdict-0-15-0.md): a cheap
navigator for short, well-named hops inside an agent flow, verified by the
agent with one snapshot - never a task closer. If it is invested in, the
highest-value adoptions from jev-ultrafast are a SELECT operation with
enumerated dropdown options, speculative operation+target heads in one API
request, and new-tab adoption; none of them lifts the humanized-speed floor
or adds reading.[^ultrafast]

[^ab]: Admin-console A/B and intended-use re-runs
[^ultrafast]: browser-use/jev-ultrafast (its docs/performance.md carries the measurement fine print)
[^code]: jev extract path (pageLines dedupe, per-line noul, typableRoles)
