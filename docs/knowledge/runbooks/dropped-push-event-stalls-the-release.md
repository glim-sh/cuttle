---
type: Runbook
title: A merge whose push event never fires stalls the release
description: GitHub can record a merge to main without creating a push event, so ci.yml - which hosts release-please and the publish jobs - never runs and no release PR appears; detect it by zero workflow runs for main's head sha, and recover with an empty ci commit pushed to main.
tags: [release, ci, github-actions, release-please]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-10-27T16:30:00+00:00" }
sources:
  - id: incident
    resource: "release of 0.17.1, 2026-10-26: PR #132 merged at 18:38:46Z as dd56f0b; the repo event feed held the PullRequestEvent 'merged' but no PushEvent, and Actions created no run for dd56f0b in 26 minutes while GitHub status reported Actions and Webhooks operational; an empty commit (8ac9b3d) started ci, release-please opened #133, and 0.17.1 published"
    title: 0.17.1 release incident
  - id: ci
    resource: /.github/workflows/ci.yml
    title: ci.yml - on push to main and pull_request only, no workflow_dispatch
---

# A merge whose push event never fires stalls the release

`ci.yml` runs on `push` to `main` and on `pull_request`; it has no
`workflow_dispatch`.[^ci] Release-please, the changelog and the publish jobs
live in it, so a merge that GitHub records without a push event leaves
everything idle: no run, no release PR, no error anywhere.[^incident]

## Detect

1. A few minutes after a merge, list the runs for main's head commit:
   `gh api "repos/glim-sh/cuttle/actions/runs?head_sha=$(gh api repos/glim-sh/cuttle/commits/main --jq .sha)" --jq .total_count`.
   `0` means nothing started. (The key is `workflow_runs`, not `runs`.)
2. Rule out the commit: no `[skip ci]`, `[ci skip]`, `[no ci]`,
   `[skip actions]` or `skip-checks: true` in its message, and `ci.yml`
   unchanged and active (`gh api repos/glim-sh/cuttle/actions/workflows`).
3. `gh api repos/glim-sh/cuttle/events` showing the PR `merged` event with no
   `PushEvent` for main confirms the push event was never created.[^incident]

## Recover

Push an empty commit straight to `main`:
`git commit --allow-empty -m "ci: retrigger the main pipeline after a dropped push event"`
then `git push origin main`. It needs a maintainer allowed to bypass the
required status checks; the push succeeds with a "required status checks are
expected" notice. The run covers every commit since the last release, so the
dropped merge is released with it, and a `ci:` commit adds nothing to the
changelog.[^incident]

Re-running an earlier run does not help: a pull-request run never executes the
release jobs, and the dropped commit has no run to re-run.

[^incident]: 0.17.1 release incident
[^ci]: ci.yml - on push to main and pull_request only, no workflow_dispatch
