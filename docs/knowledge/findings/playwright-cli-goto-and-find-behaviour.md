---
type: Finding
title: playwright-cli goto snapshots before hydration, and find prints ancestor chains
description: In the bundled playwright-cli 0.1.20, goto waits for domcontentloaded then load (5s, error swallowed) and snapshots at once - no network-idle and no option for one - so a client-rendered page reads empty right after it; and find prints every hit as its full ancestor chain plus a context window (24 hits = 367 lines), which cuttle re-matches down to one referenced line per hit.
tags: [playwright-cli, cuttle-pw, drivers, snapshot]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-21T16:30:00+00:00" }
sources:
  - id: pr112
    resource: https://github.com/glim-sh/cuttle/pull/112
    title: "docs(skill): goto snapshots at load, so a client-rendered page reads empty at first (merged 2026-09-19; the commit message carries the driver-source reading and the local repro)"
  - id: pr116
    resource: https://github.com/glim-sh/cuttle/pull/116
    title: "fix(pw): compact find output, downloads into a directory, quieter logs (merged 2026-09-19)"
  - id: skill
    resource: /packages/cuttle/internal/cli/SKILL.md
    title: embedded skill, gotcha 7 (goto returns at load) and the content-heavy-page rule
  - id: code
    resource: /packages/cuttle/internal/cli/playwright.go
    title: cuttle pw wrapper - the find re-match and the inline-verb decision
  - id: stress
    resource: "agent stress runs 2026-09-19 (session record, no durable link): two agents independently ranked find's verbosity the top friction; one reported a nested-quote eval bug that was this goto timing"
    title: Stress-run friction reports
---

# Finding

Two behaviours of the bundled `@playwright/cli` 0.1.20 that `cuttle pw`
users hit as if they were cuttle bugs. Both are the driver's, and cuttle
answers each in its own way: a documented wait for the first, a rewrite of
the output for the second.

## goto snapshots at load, before a client-rendered app has drawn

The driver's `goto` is `page.goto` at `domcontentloaded`, then
`waitForLoadState('load')` with a 5s timeout whose error is swallowed, then
an immediate snapshot. There is no network-idle wait and no option to ask
for one.[^pr112] So the snapshot printed under `goto`, and an `eval` run
right after it, see the pre-render DOM of a client-rendered page: empty
`listitem`s or containers where the content belongs, and `querySelector`
returning `null` for an element that is there a second later.[^pr112][^skill]

This was reported by a stress agent as an `eval` quoting bug - a
nested-quote `document.querySelector('a[href^="/wiki/"]')` returning
null. Reproduced against a local page that fills its list 3s after load:
the same eval returns null at once and the node 4s on, so the quoting
layer was never at fault - cuttle passes the argument verbatim on every
backend and the driver evaluates it intact.[^pr112][^stress]

The remedy is to wait for the content, not to change the driver:
`cuttle pw run-code 'async page => page.waitForSelector("...")'` or
`find '<expected text>'`, then re-snapshot. That is skill gotcha 7; there
is no code change because the driver offers nothing to configure.[^skill]

## find prints every hit as its ancestor chain

The driver's `find` is line-based over the aria snapshot: it prints each
hit as its full ancestor chain plus a window of sibling lines, and marks
no line as the match. On an encyclopedia article, `find 'Population'`
printed 24 snippets over 367 lines (16.7 KB) - the opposite of the
targeted read the skill sends agents to `find` for, and the friction two
stress agents independently ranked first.[^pr116][^stress]

Since #116 cuttle re-runs the header's query (the case-insensitive
substring, or the `--regex` with its flags) over the snippet lines and
prints each matching snapshot line once, behind its nearest referenced
ancestor, so every line carries a ref `snapshot <ref>` or `click` can
take. Same page: 26 lines, 3.6 KB. Hits are capped at 40 with a count of
the rest (`find 'the'` on the article: 40 lines and `... 814 more
matches`); lines over 200 runes are clipped with the ref kept. `--raw`,
`--json`, and a regex Go cannot compile or that re-matches nothing pass the
driver's output through untouched.[^pr116][^code]

Related driver facts: the attach model in
[playwright-cli attach and session model](/findings/playwright-cli-attach-model.md),
action verbs writing their snapshot to a file in
[playwright-cli action verbs always write their snapshot to a file](/findings/playwright-cli-action-verbs-snapshot-to-file.md),
and the client-side overlay that makes a landed page look empty in
[a client-side app transition can update URL and title while the body stays hidden](/findings/spa-transition-hides-body-behind-overlay.md).

[^pr112]: docs(skill): goto snapshots at load, so a client-rendered page reads empty at first (merged 2026-09-19; the commit message carries the driver-source reading and the local repro)
[^pr116]: fix(pw): compact find output, downloads into a directory, quieter logs (merged 2026-09-19)
[^skill]: embedded skill, gotcha 7 (goto returns at load) and the content-heavy-page rule
[^code]: cuttle pw wrapper - the find re-match and the inline-verb decision
[^stress]: Stress-run friction reports
