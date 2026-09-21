---
type: Finding
title: Stress-round verdict behind 0.15.0 - pw is the release, jev-browse is experimental
description: On 2026-09-19 six agent stress runs (opus and sonnet, a signed-in task file and a public one) passed every plain cuttle pw task, most in under a minute, while jev-browse failed 4 of 5 real search-UI chunks; the ranked friction list produced #108-#116, and the round is why 0.15.0 ships pw alone in the skill and demotes jev-browse to a --help pointer.
tags: [stress-test, release, cuttle-pw, jev-browse, skill]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-21T16:30:00+00:00" }
sources:
  - id: stress
    resource: "agent stress round 2026-09-19 (session record, no durable link): six agent runs on opus and sonnet, each given one task file - signed-in tasks on a professional-network site or public tasks on an encyclopedia and test pages - with the instruction to time every task, try jev-browse on the search-shaped chunks, and rank the friction it met; the combined verdict covers four runs on main c902c58 and two on the head before it"
    title: Stress round, 2026-09-19
  - id: fixes
    resource: https://github.com/glim-sh/cuttle/pulls?q=is%3Apr+is%3Amerged+merged%3A2026-09-19
    title: "PRs #108-#116, merged 2026-09-19 (each body names the stress run it answers)"
  - id: pr116
    resource: https://github.com/glim-sh/cuttle/pull/116
    title: "fix(pw): compact find output, downloads into a directory, quieter logs"
  - id: demote
    resource: "commit cfc05d8 on main, docs(skill): point jev-browse at its --help instead of a section (2026-09-21, in 0.15.0)"
    title: jev-browse demoted to a --help pointer
  - id: skilltest
    resource: /packages/cuttle/internal/cli/skill_test.go
    title: TestSkillGuideStaysSmall (skillBudget = 16 KiB)
  - id: memo
    resource: "jev-browse design memo, 2026-09-19 (session record, no durable link): section 4, whether to ship jev-browse in 0.15.0"
    title: jev-browse design memo, section 4
---

# Finding

## Method

Six agent runs, on opus and on sonnet, drove a live cuttle instance
through the embedded skill as an agent would in a session, each with one
task file: signed-in tasks on a professional-network site (a job search,
its filters, a company's people tab) or public tasks (an encyclopedia
article's sections, a test page's dialogs and downloads). Each agent timed
every task, ran `cuttle jev-browse` on the chunks shaped like a navigation
goal, and handed back a ranked list of the friction it met. The combined
verdict covers four runs on main at c902c58 (#110 merged) and two on the
head before it.[^stress]

## Result

Every plain `cuttle pw` task passed on both models: 5/5 and 5/5 on the
signed-in file (33-47s per task on opus, 41-105s on sonnet, the long one a
people-tab search workaround), 8/9 first try and 9/9 on the public file
(the miss was a download that needed one retry). Both models called the
`pw` path release-ready from the agent's seat.[^stress]

`cuttle jev-browse` failed 4 of the 5 real search-UI chunks: 0/2 and 0/2
on the signed-in file (exit 3, the write gate withholding the filter
panel's "Apply current filters" button), blocked at the target section on
the public article (exit 3 with the heading already on screen), and one
pass (exit 0 in 4 steps). It was the only thing that failed, on both
models.[^stress]

## Friction, ranked, and what fixed each

1. jev-browse stalls on real search UIs - #114 offered the withheld
   filter-apply controls; the "nothing makes progress" exit from a page
   that already shows the answer was the pruning #117 then removed
   ([jev-browse: the model judges, the loop acts](/decisions/jev-model-judges-loop-acts.md)).
2. `pw find` prints full ancestor chains per hit (two agents) - #116,
   one line per hit
   ([playwright-cli goto and find behaviour](/findings/playwright-cli-goto-and-find-behaviour.md)).
3. The post-`goto` snapshot and `eval` race a client-side render (two
   agents) - #112, skill gotcha 7; no code fix, the driver has no
   network-idle option.
4. `cuttle downloads` refused a directory destination - #116.
5. `down --purge` left the host snapshot dir behind (two agents) - #113.
6. `snapshot <ref>` replaces the page's live refs; `dialog-accept
   '<text>'` was undocumented - #115.
7. `cuttle logs` opened with tini's PID-1 warning and the egress IP -
   #116; the D-Bus spam went in #111.
8. Skill rule 6 still said `snapshot` prints a password in cleartext,
   after #109 masked password fields - #112.
9. The lease error prints the host's FQDN - cosmetic, left as is.

Before the round, #108 (judge done on the page that landed), #110 (host
snapshot file for `snapshot`, console line naming its verb) and #111
(`downloads --wait` accepting a finished download) had answered the
earlier runs. Every item above was merged the same day, each behind an
independent review.[^fixes][^pr116]

## Consequence: the skill carries only what every session pays for

The embedded SKILL.md is loaded at the start of every agent session, so
every line is a per-session cost for every user of the browser; a 13-15 KB
guide is roughly 4k tokens a session. A command that is experimental or
rarely used earns a pointer to its own `--help`, not a section: the
`--help` is paid for only by the session that runs the command. The
`TestSkillGuideStaysSmall` cap of 16 KiB is the hard bound, and the test's
own message says to move material to `docs/OPERATING.md` rather than raise
it.[^skilltest]

On the round's evidence, and the memo's recommendation to ship `pw` alone
and keep jev-browse experimental until a one-shot experiment passes,
0.15.0 demoted jev-browse from a ~48-line section (usage, flags, the exit
table, the write-refusal rules) to five lines pointing at `cuttle
jev-browse --help`: 15,410 to 13,088 bytes, about 700 tokens returned to
every session. The behaviour sentences moved to the command help and
`docs/OPERATING.md`. The EXPERIMENTAL label stays until jev-browse passes
the bar the memo set: the benchmark flow in four one-shot legs, five runs
each, at least 4/5 per leg, zero write clicks, wall time at or under `pw`
at low effort.[^demote][^memo]

The pre-round benchmark numbers that framed the memo are in
[jev-browse slowness was the driver path, not the model](/findings/jev-browse-slowness-was-the-driver-path.md)
and
[orchestrator reasoning effort is the largest browse-time lever](/findings/orchestrator-effort-is-the-browse-time-lever.md).

[^stress]: Stress round, 2026-09-19
[^fixes]: PRs #108-#116, merged 2026-09-19 (each body names the stress run it answers)
[^pr116]: fix(pw): compact find output, downloads into a directory, quieter logs
[^demote]: jev-browse demoted to a --help pointer
[^skilltest]: TestSkillGuideStaysSmall (skillBudget = 16 KiB)
[^memo]: jev-browse design memo, section 4
