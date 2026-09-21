---
type: Decision
title: jev-browse - the model judges the page, the loop acts on its pick
description: The jev-browse loop shows the model every interactive element plus the page's headings and text, never hides a candidate or overrides a verdict with a local rule, and guards writes on the chosen action - a hard-deny verb list plus one noul, never on a fill; a none is an honest exit 3, never an implied done.
tags: [jev-browse, decision-layer, write-guard, calibration]
status: stable
stale_after: "2027-03-21T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-21T16:30:00+00:00" }
sources:
  - id: pr117
    resource: https://github.com/glim-sh/cuttle/pull/117
    title: "refactor(jev): the model judges the page, the loop acts on its pick (merged 2026-09-19)"
  - id: pr118
    resource: https://github.com/glim-sh/cuttle/pull/118
    title: "fix(jev): a none never implies done, a fill is never refused (layer A)"
  - id: pr108
    resource: https://github.com/glim-sh/cuttle/pull/108
    title: "fix(jev): judge done on the page that landed, and end done on a none above even odds (merged; the none rule this decision removed)"
  - id: memo
    resource: "jev-browse design and reliability memos, 2026-09-19 (session record, no durable link): the analysis behind #117 and #118, quoting the pre-#117 pruning sites and the post-#117 live scores"
    title: jev-browse design and reliability memos
  - id: live
    resource: "live runs 2026-09-19..21, session scratch logs: 10 one-shot jev-browse runs on the #117 head over five task chunks (two encyclopedia sections, three job-search chunks on a signed-in professional-network site), then 16 runs comparing #118 layer A against layer A+B on six chunks; each log is the step trail with the model's done, blocked, pick and write scores"
    title: Live step logs
  - id: code
    resource: /packages/cuttle/internal/jev/decide.go
    title: jev decision layer (hard-deny list, write question, finished())
---

# Decision

`cuttle jev-browse` sends the model the whole page and acts only on what
the model picks. The model sees `page.url`, `page.title`, the headings
(capped at 40), the first ~120 text lines, and every interactive element
in document order - dialog scoping and the 500-element cap are the only
limits - plus a history whose steps carry `changed` (the page signature
moved) and `refused`. Values go out by name only; a control that echoes a
typed value is kept with the value redacted to `<<typed value>>`.[^pr117]

No local rule may remove a candidate or flip the model's verdict. The loop
decides exactly four things: act on the pick; stop on `done >= 0.8`; stop
on `blocked >= 0.8`; guard the pick against writes. A `none` exits 3 at the
confidence the model gave it, "the model found no useful action on this
page", and never implies done.[^pr117][^pr118]

The write guard runs on the CHOSEN action, not on the offer list. First a
hard-deny list of irreversible verbs matched as whole words in the
control's own name (pay, buy, purchase, checkout, place order, transfer,
donate, delete, remove, send, post, publish, sign out, unsubscribe; a link
or tab only when its name leads with the verb), which no judgement
overrides. Then one noul per pick - does taking it change something on the
site rather than navigate, open, sort or filter - refused at p >= 0.2.
`back` and a `type:` pick are exempt: typing changes nothing on a site,
the submit does. A refusal is recorded in history and the model re-picks;
a second refusal on the same page exits 3 with "the task needs a write
action: <label>" and the `cuttle pw` handoff, so a person does the write
deliberately.[^pr117][^pr118][^code]

## Why

The stress round's jev-browse failures were not model errors. Before #117
the loop pruned what the model could see and pick - a write-verb regex
withheld a filter panel's "Apply current filters" button, `tried` and
`failedOn` removed a toggle and a link after one failed click,
`sectionAnchor` dropped the table-of-contents link that is the route on
an article, `typedHere` withdrew Enter - and then asked the model to judge
a page whose text and headings it was never shown. The model was handed an
empty room and answered `none`; the loop reported "nothing on this page
makes progress" from the page it had been asked to reach.[^memo][^pr117]

Guarding the pick instead of the list costs one model call per acted step
(about +0.5-0.7s on a ~3.5s humanized click) and is what tells a filter
apply from a job application: "apply" is deliberately not on the hard
list, because as a whole word it would deny the apply button the memo's
own fixture expects to be clicked.[^pr117]

#118 layer A closed two holes the first live runs on #117 exposed: the
`none && done > blocked` clause in `finished()` (#108's "a none above even
odds ends done")[^pr108] went, and the write noul is no longer asked about
a fill. The evidence for both, and for two proposals that were measured
and not taken, is below.[^pr118][^live]

## Evidence

**A none with done > blocked is a coin flip.** `blocked` sits at 0.03-0.09
on every live page, so `none && done > blocked` reads as "a none implies
done". In the 10 one-shot runs on the #117 head that clause ended four:
twice correctly on an encyclopedia section, twice falsely on a job-search
page, at done 0.61 and 0.63, with a value typed into the wrong box and a
global search run instead of the job search. Correct terminal pages do not
score higher in a way a threshold could use: a correctly filtered job page
drew done 0.64-0.70, an article section 0.82-0.94, so no cut separates the
false dones from the honest low scores. The clause was deleted; a none is
always exit 3 at the model's own confidence.[^memo][^live][^pr118]

**A noul cannot judge a redacted value.** The reliability memo proposed
replacing `done` with `reached` plus an `applied` noul ("does the page
reflect every typed value"). Live on #117, `applied` scored 0.67-0.73 with
the value visibly in effect - the box showing the city, the address
carrying its geo id - because the model sees `<<typed value>>` where the
value would be, and `reached` sat at 0.77-0.79 on a correct terminal page
twice. Whether a box was submitted is therefore a loop-owned fact, not a
question for the model. #118 layer B built that fact (`applied` /
`pending` state and a loop-side veto on done while a value is unsubmitted)
and in 16 A-vs-B runs the veto never fired: removing the none rule had
already prevented the false done, and both layers still exited 3 on the
same finished filter pages. Layer B was dropped as ~100 lines that rescued
nothing.[^memo][^pr118][^live]

**The write noul straddles the threshold on submits.** With a
role-naming question ("is the searchbox named ... a control that changes
something on the site when activated"), pressing Enter and a form's own
Search button scored 0.20-0.27 against `writeThreshold` 0.2 and were
refused nine times across the runs; typing into a box scored 0.20-0.33. A
chat overlay's Close button drew 0.22 with the shipped wording and ended a
run on the second refusal. The wording moves the scores across the
threshold, and the threshold is not a knob to tune per page: the fix was
to never ask about a fill and to keep the shipped question, not to raise
0.2.[^pr118][^live]

**Fill confidence does not separate right from wrong.** The memo's floor
on value-consuming picks (exit 3 below 0.4) assumed correct fills at
0.43-0.98 and wrong ones at 0.21-0.38. Live, correct fills on the
signed-in site scored 0.35-0.50 - on the #118 logs a correct search-box
fill drew 0.32 and a correct Enter 0.27 - while the wrong fills drew
0.24-0.28. A 0.4 floor would have stopped four correct runs; no floor
separates the two, so none is applied.[^memo][^pr118][^live]

## Consequences

- Chunk tasks by page, not by value: one goal, one page transition per
  run, and a two-value search started on the page that shows both boxes.
  The model cannot tell a value typed into the wrong box from one applied,
  and a none on a landing page is the loop saying so.
- A page whose only route is a write ends with exit 3 naming it; that is
  the design, and `cuttle pw` is where the write happens.
- The mock transport never answers `none` or a non-zero noul, so these
  paths are covered by the scripted-transport fixtures in
  `internal/jev/testdata/`, not by `--mock` runs.[^pr117]

The pre-#117 per-cause fixes and their timings are in
[jev-browse slowness was the driver path, not the model](/findings/jev-browse-slowness-was-the-driver-path.md);
the redaction rule in
[the aria snapshot renders field values](/findings/aria-snapshot-renders-secret-values.md).

[^pr117]: refactor(jev): the model judges the page, the loop acts on its pick (merged 2026-09-19)
[^pr118]: fix(jev): a none never implies done, a fill is never refused (layer A)
[^memo]: jev-browse design and reliability memos
[^live]: Live step logs
[^pr108]: fix(jev): judge done on the page that landed, and end done on a none above even odds (merged; the none rule this decision removed)
[^code]: jev decision layer (hard-deny list, write question, finished())
