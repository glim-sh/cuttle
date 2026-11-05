---
type: Runbook
title: A release tag with no image or binaries
description: A tag and GitHub release can exist with no GHCR image, binaries or cask when the publish steps of the run that cut it fail; re-running cannot publish it, because release-please reports a release only in the run that creates it, so the recovery is cutting the next patch release - and a merged release PR left untagged is recovered by re-running its own commit's run.
tags: [release, ci, github-actions, release-please, ghcr]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-05T12:00:00+00:00" }
sources:
  - id: incident
    resource: "release of 0.17.3, 2026-11-04: release PR #152 merged at 22:26:44Z as 715fdb9 and docs PR #153 a minute later as 1d76b99; 1d76b99's push run cancelled 715fdb9's build-and-test and image-arm64 (concurrency keyed on github.ref, cancel-in-progress), so 715fdb9's release job was skipped; 1d76b99's release job then tagged v0.17.3 at 715fdb9 and GoReleaser failed with 'git tag v0.17.3 was not made against commit 1d76b99'; re-running 715fdb9's run built nothing; v0.17.3 was left with zero release assets and no ghcr.io/glim-sh/cuttle:0.17.3"
    title: 0.17.3 release incident
  - id: ci
    resource: /.github/workflows/ci.yml
    title: ci.yml - concurrency, the release-commit guard, release-image gated on release_created
---

# A release tag with no image or binaries

`ci.yml` cuts and publishes a release in one run: release-please tags the
merged release PR, then GoReleaser uploads the binaries and cask and
`release-image` pushes the GHCR image, both gated on release-please's
`release_created` output.[^ci] That output is true only in the run that
creates the release. If the publish steps after the tag fail, the tag and the
GitHub release stay, and every later run - a re-run of the same one included -
sees the release already made and publishes nothing.[^incident]

## Detect

1. The release run's `release` or `release-image` job is red, or the release
   commit's run was cancelled.
2. `gh release view vX.Y.Z --repo glim-sh/cuttle --json assets --jq '.assets | length'`
   prints `0`, and `docker manifest inspect ghcr.io/glim-sh/cuttle:X.Y.Z`
   fails while the previous version's succeeds.

## Recover

Cut the next patch release. Land any `fix` commit (a `fix(ci)` that removes
the cause is the natural one); release-please opens `chore(main): release
X.Y.Z+1`, and merging it runs a full publish from its own commit. Leave the
broken version as it is: deleting and recreating its tag would rewrite a
published ref.

Re-running the failed run does not help: its release job runs release-please
again, which finds the release already cut and reports `release_created`
false, so GoReleaser and `release-image` skip.[^incident]

## The cause that is closed off

For 0.17.3 a later merge's push run cancelled the release commit's build,
then cut the release itself and failed, because GoReleaser and the image build
publish the commit the run checked out.[^incident] Main's runs no longer
cancel one another, and the release job cuts a release only when its run's tree
is the release commit's - that commit's own run, or an empty commit pushed onto
it after [a dropped push event](/runbooks/dropped-push-event-stalls-the-release.md).[^ci]

That guard has its own failure mode: if the release commit's own run never
reaches release-please (a red build, a cancelled run), the merged release PR
stays labelled `autorelease: pending`, later runs warn that they are leaving
it to that commit's run, and release-please opens no new release PR. Re-run
that commit's run (after a transient failure or a cancellation): the re-run
cuts and publishes the release. The same applies if GitHub drops the release job while
it is pending, which it does when two more main runs queue behind a running
release job.[^ci]

[^incident]: 0.17.3 release incident
[^ci]: ci.yml - concurrency, the release-commit guard, release-image gated on release_created
