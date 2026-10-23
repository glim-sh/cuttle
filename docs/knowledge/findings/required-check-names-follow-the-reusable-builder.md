---
type: Finding
title: A required check named after a runner label breaks when the reusable builder moves runners
description: The arm64 image job runs through docker/github-builder pinned at @v1, whose check name embeds the runner label; when the builder moved from ubuntu-24.04-arm to ubuntu-26.04-arm, main's ruleset kept requiring the old name, so every PR sat BLOCKED with all checks green. The ruleset now requires image-arm64 / finalize, whose name carries no runner label.
tags: [ci, github, ruleset, release]
status: stable
stale_after: "2027-10-23T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-10-23T21:10:00+00:00" }
sources:
  - id: pr
    resource: https://github.com/glim-sh/cuttle/pull/129
    title: PR 129 - green CI, mergeStateStatus BLOCKED until the ruleset was fixed
  - id: ci
    resource: /.github/workflows/ci.yml
    title: ci.yml image-arm64 job (uses docker/github-builder/.github/workflows/build.yml@v1)
  - id: ruleset
    resource: "main's branch ruleset (repository settings, read via gh api repos/glim-sh/cuttle/rules/branches/main), observed 2026-10-23"
    title: main ruleset required_status_checks
---

# A required check named after a runner label breaks when the reusable builder moves runners

The `image-arm64` job is a call to Docker's reusable workflow,
`docker/github-builder/.github/workflows/build.yml@v1`.[^ci] Its build job is a
matrix, and GitHub names the check after the matrix values, runner label
included: `image-arm64 / build (0, linux/arm64, ubuntu-24.04-arm)`. The `@v1`
tag floats, and the builder moved that job to `ubuntu-26.04-arm`, so the check
started reporting as `... ubuntu-26.04-arm)` while main's ruleset still required
the `ubuntu-24.04-arm` name.[^ruleset]

A required check that never reports does not fail - it leaves the PR waiting.
Every check on the PR was green and `mergeStateStatus` was `BLOCKED`, with
nothing on the PR page naming the cause.[^pr] It blocks every PR to main at
once, not the one that happens to surface it.

The ruleset now requires `image-arm64 / finalize` instead, together with
`build-and-test` and `release-note-lint`. `finalize` runs after the per-platform
builds and its name carries no matrix or runner value, so a runner move upstream
cannot rename it.[^ruleset] The same rule applies to any check required from a
reusable or matrix workflow: require a job whose name the repo controls, not
one derived from someone else's matrix.

When a PR is green but `BLOCKED`, compare the ruleset's required contexts
(`gh api repos/glim-sh/cuttle/rules/branches/main`) with the check names the run
actually reported (`gh pr checks <n>`) before suspecting reviews or signatures.

[^ci]: ci.yml image-arm64 job (uses docker/github-builder/.github/workflows/build.yml@v1)
[^ruleset]: main ruleset required_status_checks
[^pr]: PR 129 - green CI, mergeStateStatus BLOCKED until the ruleset was fixed
