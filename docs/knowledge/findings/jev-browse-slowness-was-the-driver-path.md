---
type: Finding
title: jev-browse slowness was the driver path, not the model
description: jev-browse ran a multi-page flow at nearly twice plain cuttle pw's time because of how it drove the page - modal-blind offers, dead-click repeats, missing Enter, redundant snapshots and spawns, section-less labels - and reached parity with pw at high effort once those were fixed and merged.
tags: [jev-browse, benchmark, performance, snapshot]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-21T16:30:00+00:00" }
sources:
  - id: bench
    resource: "benchmark harness runs on 2026-09-18/19 against a signed-in site: the same flow as the orchestrator-effort benchmark, humanize on, jev-browse driven in chunks by a claude -p orchestrator, plus one one-shot scripted run (session record, no durable link)"
    title: jev-browse benchmark runs
  - id: main
    resource: "benchmark harness rerun on 2026-09-19 12:29-13:05 UTC+1 on merged main at 9ca9386 (#100 + #102 + #103), same flow, jev-browse driven by a claude -p orchestrator (Opus) at low and high effort, 2 runs each (session record, no durable link)"
    title: Merged-main benchmark rerun
  - id: pr
    resource: https://github.com/glim-sh/cuttle/pull/102
    title: "feat(jev): withhold write actions and halve jev-browse run time (merged)"
---

# Finding

Before the fixes, an LLM orchestrating `cuttle jev-browse` in chunks took
282-348s on the flow that plain `cuttle pw` did in 158-187s at the same
effort; a one-shot scripted run failed at 1007s with 135 of 138 clicks
failing.[^bench] The model's decision time was not the cause. The root
causes were all in how jev drove the page:[^bench]

1. **Modal-blind offers.** With an in-page modal open, jev still offered
   the elements behind it; every such click timed out after 5000ms.
2. **Dead-click repeats.** It re-picked an autocomplete option whose click
   had just failed, on an unchanged page.
3. **No Enter after typing** into a combobox, so a search never submitted.
4. **Driver cost per step.** About 2 snapshots and 3-8 process spawns per
   step, ~4.3s/step before any humanized action.
5. **Section-less labels.** Offered labels did not name the page section,
   so search values went to the site-wide search box instead of the form's.

Fixes, one per cause: scope offers to the open modal, exclude a click that
left the page unchanged, offer Enter after typing, take one snapshot after a
navigation and keep one persistent driver client per run, and label options
with their section and filled state.[^pr] The same change also withholds
write-shaped actions behind a deny gate - a safety fix, not a speed one.
After them: 157s/170s at high effort -
level with plain pw at about half its cost - and 111s/120s at low effort,
against pw's 101s/103s.[^bench]

## On merged main

With #102 merged, the rerun on main gave 163s and 134s at high effort,
2/2 passing - parity with plain pw's 148-161s on the same day, and less
than half the pre-fix 282-348s. At low effort it ran 95s and 167s but
passed 0/2, both failing the same "people" step.[^main]

Those failures are not a regression from #102 or #103: every arm, the
pre-#102 jev build included, met a site-side overlay on that step, and
the low-effort orchestrator was the one that read `innerText` and gave up
instead of taking a snapshot or reloading. jev-browse's own done-check has
a related, pre-existing weakness - it trusts URL and title while the DOM
still holds the previous page - detailed in
[a client-side app transition can update URL and title while the body stays hidden](/findings/spa-transition-hides-body-behind-overlay.md).
So the honest reading is: jev-browse is at parity with pw at high effort,
and pw at low effort remains the fastest reliable configuration.[^main]

The modal facts behind fix 1 are in
[aria snapshot: focus and modal shape](/findings/aria-snapshot-focus-and-modal-shape.md);
the per-spawn cost behind fix 4 in
[cost of one cuttle pw call](/findings/pw-call-cost-breakdown.md).

## Note, 2026-09-21

The candidate withholding described above (the write deny gate, the
exclusion of a click that left the page unchanged, and the rest of the
pruning #102 introduced) is history: #117 replaced pruning the offer list
with a guard on the chosen action, after the stress round showed the
pruning handing the model an empty room. The timings above were measured
on the pruning design and stand as recorded; the current design and its
evidence are in
[jev-browse: the model judges the page, the loop acts on its pick](/decisions/jev-model-judges-loop-acts.md).

[^bench]: jev-browse benchmark runs
[^main]: Merged-main benchmark rerun
[^pr]: feat(jev): withhold write actions and halve jev-browse run time (merged)
