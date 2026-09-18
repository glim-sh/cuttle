# jev-step

Plan once, loop cheap.

An LLM is very good at reading a task and writing down how to do it. It is a
slow, expensive way to answer "which of these 40 buttons do I press next",
which is the question a browse or a UI test asks a few dozen times per run.

`jev-step` splits the two. The LLM writes a plan file once: the milestones, what
each one should look like when it lands, and every value that may be typed. From
there each step is one call to [Jev](https://docs.typesafe.ai/introduction), a
model that takes structured state and returns a typed choice with a calibrated
confidence in roughly 100-300ms and generates no text at all. The page's
interactive elements go in, an action comes out, `playwright-cli` performs it.

It is not shipped in a release or in the image yet. Build it from the repo root
with `go -C packages/cuttle build -o jev-step ./cmd/jev-step`, then:

```bash
jev-step --goal "sign in and export the invoice list" --plan plan.json --loop
```

The approach is not speculative: [jev-browse](https://github.com/kyrylosyzonenko/jev-browse)
benchmarked the same loop against an LLM driving the browser directly and
reports finishing the same 30 tasks at roughly half the wall clock for about
1/70th of the cost. Nothing here reproduces that measurement.

## The plan file

```json
{
  "steps": [
    {
      "description": "sign in with the test account",
      "expect": "the account menu is visible",
      "fill": {
        "username": "qa@example.com",
        "password": "{{cuttle:QA_PASS}}"
      }
    },
    {
      "description": "open the invoices page and export it",
      "expect": "a CSV download has started"
    }
  ]
}
```

`expect` is what tells the loop a step has landed, so write it as something
visible on the page rather than as a feeling. `fill` values are typed verbatim,
which is what makes cuttle's secret sentinels work: hand the value to cuttle
once (`cuttle secret set QA_PASS --stdin`) and put `{{cuttle:QA_PASS}}` in the
plan. It is passed through untouched and substituted inside cuttle, on the fill
path, so the real value never enters this process, the driver's argv, or the
model call.

## Exit codes

The exit code is the result. Nothing worth branching on is buried in the output.

| Code | Meaning |
| --- | --- |
| `0` | An action was taken on the page. Run it again with the same `--step`. |
| `1` | A usage or infrastructure error. The message says which. |
| `2` | The goal is reached. |
| `3` | Escalate to a human or to the planning LLM. |
| `4` | This plan step is complete. Run it again with the next `--step`. |

Code `3` covers every "a person should look at this" case: the decision came
back under `--confidence-threshold`, Jev chose `escalate` outright, the chosen
element is not one this page offered, the page has nothing to act on, `--loop`
hit `--max-steps`, or the snapshot carries a `### Modal state` line, meaning a
native dialog has parked the renderer and every read after it would be of a
stale page.

Nothing is journalled between invocations, so a re-run after code `0` decides
afresh against whatever the page now shows. If the action that earned that `0`
submitted something, re-running is how it gets submitted twice.

## Flags

| Flag | Default | |
| --- | --- | --- |
| `--goal` | | required: what the whole run is for, in one sentence |
| `--plan` | | required: path to the plan file |
| `--step` | `0` | zero-based index of the plan step to work on |
| `--confidence-threshold` | `0.7` | escalate below this, and do not call a step complete below it either |
| `--loop` | off | keep going until done, escalation, or `--max-steps` |
| `--max-steps` | `20` | the loop's budget, in actions |
| `--mock` | off | decide locally, without Jev or an API key |

The API key comes from `JEV_API_KEY`. `playwright-cli` is taken from `PATH`.

`--mock` picks the first plausible element instead of asking anything. It is not
a stand-in for Jev's judgement: it exists so the snapshot parsing, the element
filtering, the driver shell-out and the exit codes can be exercised end to end
while Jev access is waitlisted. Only the judgement is mocked - it clicks and
fills the live page for real, so point it at a page you are willing to have it
press the first button on.

## What is sent to TypeSafe

Only the goal, the current plan step's `description` and `expect`, the page URL
and title, and one line per interactive element: `{id, role, label}` - the first
255 of them, which is Jev's cap on the options in a Choice question.

Never sent: page body text, the contents of any field, and the plan's fill
values. When Jev chooses to type, it chooses among the fill NAMES ("password"),
and the value behind that name is looked up here, after the answer comes back.
The test that pins this down is `TestBuildRequestSendsNamesNotValues`.

`description` and `expect` are the half of this the code cannot enforce: they go
over verbatim, so keep credentials out of the prose the planning LLM writes into
them. Only `fill` is protected by the name indirection.

Element labels are authored by the page. They are treated as data throughout:
quoted into the log, sent as Choice option descriptions, and never followed as
instructions.

## Tests

```bash
go -C packages/cuttle test ./cmd/jev-step/
```

`just check` covers it too - it is an ordinary package of the cuttle module.

The snapshot parser is tested against real captured `playwright-cli snapshot`
output in `testdata/`, including the frame-qualified refs (`f1e10`) that appear
after a navigation, a disabled control, an escaped quote inside an accessible
name, and the error-plus-modal output a pending dialog produces. The loop tests
put a stub `playwright-cli` on `PATH` and assert the exit code for each outcome,
and the exact driver call for the one that types a secret sentinel.
