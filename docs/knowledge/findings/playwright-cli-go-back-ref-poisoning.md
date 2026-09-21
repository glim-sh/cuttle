---
type: Finding
title: playwright-cli go-back leaves the snapshot emitting dead refs
description: After go-back, playwright-cli's snapshot refs are dead because Chrome restored the page from the back/forward cache, which Playwright does not support; the fix is launching Chrome with --disable-back-forward-cache, which cuttle now does, so the jev re-goto workaround is gone.
tags: [playwright-cli, drivers, jev-browse, upstream-bug, launch-flags]
status: stable
stale_after: "2027-03-18T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-21T16:31:00+00:00" }
sources:
  - id: code
    resource: /packages/cuttle/internal/fingerprint/args.go
    title: baseChromeArgs, the flags the daemon launches every Chrome with
  - id: live
    resource: "live repro transcripts, 2026-09-18: first through cuttle pw, then standalone playwright-cli attached over --cdp to a plain headless Chrome, no cuttle code in the path (session record, no durable link)"
    title: Live repro
  - id: upstream
    resource: https://github.com/microsoft/playwright/issues/42777
    title: Upstream report and maintainer verdict
  - id: pwdocs
    resource: https://playwright.dev/docs/navigations#backforward-cache-bfcache
    title: Playwright navigations docs, back/forward cache
---

# Finding

In `@playwright/cli` 0.1.20 - the pinned bundled driver - against a Chrome
with its back/forward cache on (the default), a `go-back` leaves the
snapshot emitting refs from the pre-navigation frame generation. Every click
on those refs fails with `Ref ... not found in the current page snapshot`.
Taking more snapshots does not recover; only a fresh `goto` re-mints refs
that work.[^live] It shows once refs are frame-qualified
(`fNeM`): the snapshot after `go-back` reuses the pre-navigation generation
number instead of minting a new one. On the first page of a fresh session,
with unprefixed refs, a click after `go-back` worked.[^live][^upstream]

It reproduces standalone over a CDP-attached session with no cuttle code in
the path, so it is not a mux artifact.[^live][^upstream]

## Upstream verdict

Reported as microsoft/playwright#42777 on 2026-09-18 and closed "not planned"
the next day: the page came back from Chrome's back/forward cache, and
Playwright does not support bfcache-restored pages at all - its own launcher
passes `--disable-back-forward-cache`, so a Playwright-launched Chrome never
hits this. A browser attached over CDP has whatever flags its launcher chose,
and the maintainers' answer is to launch it with that flag.[^upstream][^pwdocs]

## What cuttle does

cuttle launches Chrome itself, so `--disable-back-forward-cache` is in
`baseChromeArgs` next to the other switches Playwright would normally add,
and the golden carries it.[^code] With the cache off, `go-back` is a full
navigation, the snapshot after it mints fresh refs, and clicks on them work
- verified live through `cuttle pw` against the image built with the flag.

The cuttle-side workaround this finding used to describe - jev-browse
re-`goto`ing the landed URL after every `back`, guarded by a test pinned to
the driver version - was removed in favour of the launch flag, along with the
SKILL.md advice to `goto` instead of `go-back`. If refs ever die after
`go-back` again, check the flag is still in the launch line before suspecting
the driver.

Related: [playwright-cli attach and session model](/findings/playwright-cli-attach-model.md),
[playwright-cli is the driver interface](/decisions/playwright-cli-is-the-driver-interface.md).

[^code]: baseChromeArgs, the flags the daemon launches every Chrome with
[^live]: Live repro
[^upstream]: Upstream report and maintainer verdict
[^pwdocs]: Playwright navigations docs, back/forward cache
