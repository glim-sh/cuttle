---
type: Reference
title: CLI surfaces of the open-source jev-browser projects
description: How a dozen public decision-model browsing agents shape their command line - goal input, values, extraction, output, exit codes, session model, key handling - and which of those choices cuttle jev-browse took or refused.
tags: [jev-browse, cli, agent-experience, ecosystem]
status: stable
stale_after: "2027-03-18T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-18T16:16:55+00:00" }
sources:
  - id: survey
    resource: "session research, 2026-09-18: twelve open-source decision-model browsing agents read at source level from local clones (survey document, no durable link)"
    title: Ecosystem survey
  - id: ax
    resource: "session design discussion, 2026-09-18: why cuttle jev-browse uses named flags and exit codes as its machine-facing API (no durable link)"
    title: Agent-experience design discussion
  - id: turbo
    resource: https://github.com/sightmap/jev-turbo
    title: sightmap/jev-turbo
  - id: kyrylo
    resource: https://github.com/kyrylosyzonenko/jev-browse
    title: kyrylosyzonenko/jev-browse
  - id: jkudish
    resource: https://github.com/jkudish/jev-browser
    title: jkudish/jev-browser
  - id: yingkai
    resource: https://github.com/ying-kai-liao/jev-browser
    title: ying-kai-liao/jev-browser
  - id: tontoko
    resource: https://github.com/tontoko/jev-browser
    title: tontoko/jev-browser
  - id: pilot
    resource: https://github.com/aidil2105/jev-browser-pilot
    title: aidil2105/jev-browser-pilot
  - id: jasonduncan
    resource: https://github.com/jasonduncan/jev-browser
    title: jasonduncan/jev-browser
  - id: ego
    resource: https://github.com/romaluev/jev-ego
    title: romaluev/jev-ego
  - id: voice
    resource: https://github.com/moritzkremb/jev-voice-browser
    title: moritzkremb/jev-voice-browser
  - id: ulka
    resource: https://github.com/razaanstha/ulka
    title: razaanstha/ulka
  - id: cuttlecli
    resource: /packages/cuttle/internal/cli/jevbrowse.go
    title: cuttle jev-browse command
  - id: cuttleloop
    resource: /packages/cuttle/internal/jev/loop.go
    title: cuttle jev-browse loop and exit codes
  - id: skill
    resource: /packages/cuttle/internal/cli/SKILL.md
    title: cuttle embedded SKILL.md
---

# Reference

A dozen public projects built the same thing at the same time: a loop where a
bounded-choice decision model picks the next element and code performs it.
Their loops converged; their command lines did not. This records the shapes
that exist, because the interface is what another agent has to hold, and
which of them `cuttle jev-browse` took.[^survey]

## The goal: positional or named

Both halves of the ecosystem are populated. Positional: `jev-browse
"<goal>" [start-url]`,[^kyrylo] `jev-browser run "<task>" <start-url>`,[^jkudish]
`jev-browser do <url> "<goal>"`.[^yingkai] Named: `--goal` in
jev-turbo,[^turbo] jev-ego[^ego] and pilot.[^pilot]

The positional form reads better by hand and is worse to generate. A bare
free-text argument sits next to `--url`, `--max-steps` and repeated
`name=value` pairs, so a quoting slip turns the second word of the goal into
an unrecognized flag; a named flag fails loudly instead. It also makes a
transcript self-describing: `--task "log into the portal"` tells a reviewer
what was asked, a bare string does not.[^ax]

## Values a run may type

Every project that types at all supplies the values out of band, because the
model chooses and never writes text. Three encodings: repeatable
`--text name=value`,[^kyrylo] repeatable `--value key=text`,[^turbo] and a
`--values` JSON object whose entries become JSON-Pointer bindings.[^tontoko]
ying-kai takes bare `key=value` positionals after the goal.[^yingkai] Two
projects gate typing behind a flag instead - `--no-typing` on by
default,[^jkudish] `--allow-typing` off by default.[^pilot]

The privacy boundary splits the field. tontoko sends only the binding path
and fills the literal locally, and redacts any echo of a supplied value out
of the outgoing request.[^tontoko] Others put the values in the state the
model receives.[^survey] Whatever the encoding, the value arrives on argv or
in a file, so it is visible to anything that can read the process list.

## Extraction and the final answer

A pick-only model cannot compose an answer, so the answer has to be
assembled from the page. kyrylo's `--extract "what to return"` flattens the
page to unique lines and asks one yes/no question per line, printing the
matches verbatim - no second model anywhere in it.[^kyrylo] tontoko takes
`--fields` and `--schema` and returns structured JSON whose every value is
copied from the DOM.[^tontoko] jkudish instead dumps the whole final page in
one of four formats (`--format text|markdown|html|aria`, with
`--max-chars`).[^jkudish] jasonduncan's tool does not browse at all: `select`
reads a request on stdin and writes the chosen element to stdout, leaving
perception, action and looping to the caller.[^jasonduncan]

## Output and exit codes

Machine-readable output is near universal but arrives in three ways: a
`--json` flag,[^turbo][^ego][^yingkai][^pilot] JSON printed
unconditionally,[^jkudish][^jasonduncan] and one JSON line per result with a
`{ok, result}` envelope on every command.[^tontoko]

Exit codes are where the interfaces really differ, and most of them throw
away the distinction that matters:

- jev-turbo: 2 usage, 1 everything else - a goal not reached exits the same
  as a crash.[^turbo]
- pilot: 0 ok, 1 goal unreached, 2 error.[^pilot]
- tontoko: 0 ok, 1 error, 2 stopped, unverified or a pending dialog.[^tontoko]
- kyrylo: 0 reached, 3 the goal needs an action the agent cannot do, 1 no
  useful action or step limit, 2 usage, 4 crash.[^kyrylo]
- ying-kai: 0 when every step passed, else 1.[^yingkai]

Only kyrylo gives "a human is needed" its own code. Nobody separates "the
step budget ran out" from "stuck", although those call for different
recoveries: one is retry with a bigger budget, the other is take over.

## Session and handoff

Almost all of them launch a browser per invocation and close it at the end,
so a run that stops halfway leaves nothing to continue from.[^survey]
tontoko is the exception with `--session NAME`, a persistent session local
to the working directory that its own verbs (`open`, `click`, `fill`, `act`,
`extract`, `close`) share; it also exposes `session`, reading JSONL commands
from stdin, and an MCP stdio server over the same core.[^tontoko] voice-browser
is the only one whose flags point at a browser someone else started
(`--cdp ws://...`), and it is a local server rather than a
one-shot command.[^voice] ulka has no command line at all - it is a browser
extension.[^ulka]

## Key handling

The env var is effectively unanimous: `TYPESAFE_API_KEY`, read by eleven of
the twelve. Two of those also accept `JEV_API_KEY`.[^survey][^tontoko][^voice]
Offline modes are common enough to count as expected: a picker or provider
selector with a `mock` or `scripted` value.[^turbo][^pilot]

## What cuttle took

`cuttle jev-browse` is `--task` (required), optional `--url`, `--max-steps`,
`--extract`, repeatable `--text name=value`, `--json` and `--mock`, exiting
0 done, 1 error, 3 blocked, 4 budget spent.[^cuttlecli][^cuttleloop]

- **Named flags throughout, no positional sugar.** The primary caller is
  another agent composing a command from a template, and named arguments are
  what a generator gets right.[^ax]
- **`--task` and `--extract` stay two strings.** They have different
  lifetimes: the task is in every decision the loop makes, the extract
  criteria runs once against the page the run ended on. One combined string
  would force the model to guess where navigation intent stops and harvest
  criteria begins.[^ax]
- **kyrylo's `--text name=value`** over tontoko's JSON bindings: the same
  names-only boundary with far less surface. Only the name reaches the API;
  the value is looked up locally on the fill path, which is what lets a
  `{{cuttle:NAME}}` secret sentinel survive as a sentinel until cuttle
  substitutes it inside its own CDP frame.[^cuttlecli] No surveyed project
  has an equivalent, so all of them put the literal on argv; cuttle's help
  says outright that a `--text` value is argv and shows in `ps`.[^cuttlecli]
- **kyrylo's verbatim-line `--extract`**, not a second model writing prose
  and not a whole-page dump.[^cuttlecli]
- **Exit codes as the machine-facing API**, with the split nobody else
  makes: 3 means a person is needed, 4 means the budget ran out. An agent
  branches on the code instead of parsing prose.[^ax][^cuttleloop]
- **`--mock`**, matching the ecosystem's offline pickers: it decides locally
  without judgement but still acts on the live page.[^cuttlecli]

## What cuttle refused

- **Launching a browser for the run.** Every outcome, including blocked and
  out-of-budget, leaves the session live on the page it stopped at, and the
  caller continues with plain `cuttle pw` verbs from there. That is the whole
  handoff design and no surveyed project has it: their runs dead-end, and
  tontoko's `--session` is a session of its own tool rather than a warm
  browser a human can also take over through the viewer.[^skill][^survey]
- **A repeatable `--goal` chaining several goals in one run.**[^pilot] One
  task per run; chaining is the calling agent's job, and it already has exit
  codes to branch on.
- **Output-format knobs** - four page formats, character caps, screenshot
  and video flags.[^jkudish] One readable step log, plus `--json`.
- **The bare `TYPESAFE_API_KEY` name.** cuttle reads
  `CUTTLE_TYPESAFE_API_KEY`, because every other environment variable it
  reads is `CUTTLE_`-prefixed and a container inherits whatever the host
  sets. The cost is real and one-sided: a user who already exported
  `TYPESAFE_API_KEY` for another tool has to set cuttle's name too.[^cuttlecli]
- **A stdin-in, stdout-out single decision** as the shipped surface.[^jasonduncan]
  A per-invocation decision cannot carry the step-to-step memory - what was
  already tried, whether the last action changed anything - that separates
  the loops that finish from the loops that spin.[^survey]

Every repo here was days old when it was read, so treat the shapes as
evidence of what designers converge on, not as maintained interfaces.

Related: [playwright-cli is the driver interface for higher-level automation](/decisions/playwright-cli-is-the-driver-interface.md),
[TypeSafe Jev API transports and answer shapes](/findings/jev-api-transports.md).

[^survey]: Ecosystem survey
[^ax]: Agent-experience design discussion
[^turbo]: sightmap/jev-turbo
[^kyrylo]: kyrylosyzonenko/jev-browse
[^jkudish]: jkudish/jev-browser
[^yingkai]: ying-kai-liao/jev-browser
[^tontoko]: tontoko/jev-browser
[^pilot]: aidil2105/jev-browser-pilot
[^jasonduncan]: jasonduncan/jev-browser
[^ego]: romaluev/jev-ego
[^voice]: moritzkremb/jev-voice-browser
[^ulka]: razaanstha/ulka
[^cuttlecli]: cuttle jev-browse command
[^cuttleloop]: cuttle jev-browse loop and exit codes
[^skill]: cuttle embedded SKILL.md
