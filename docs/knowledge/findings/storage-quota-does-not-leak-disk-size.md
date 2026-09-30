---
type: Finding
title: Stock Chrome's storage quota does not leak disk size, so it is not overridden
description: Stock Chrome 154 reports navigator.storage.estimate() quota as usage plus about 10 GiB, independent of the disk, so it needs no override; the former quota-override patch could only create the tell it was meant to hide and was removed.
tags: [stealth, storage, quota]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T20:49:00+00:00" }
sources:
  - id: plan
    resource: /docs/plans/2607-23-self-hosted-chromium-build-pipeline.md
    title: Build-pipeline plan - measured quota grows as 10 GiB + usage, byte-for-byte stock
  - id: p37
    resource: /packages/browser/patches/0037-incognito-static-quota.patch
    title: Patch 0037 - kIncognitoStaticStorageQuota is on by default upstream since 154
  - id: removal
    resource: "git history: commit 'fix(browser): drop the storage-estimate quota override patch', which deleted packages/browser/patches/0036-storage-estimate-from-cli.patch"
    title: Removal of the quota-override patch
---

# Stock Chrome's storage quota does not leak disk size

Measured, stock Chrome reports `navigator.storage.estimate()` quota as usage
plus about 10 GiB, so the number says nothing about the disk it runs
on.[^plan] Upstream also enables `kIncognitoStaticStorageQuota` by default
since 154.[^p37] There is nothing to spoof.

cuttle used to carry a patch that overrode the estimate from a
`--fingerprint-storage-quota` switch. The daemon never passed it, and when set
it answered differently from stock Chrome: it ran before the opaque-origin
TypeError, invented a fixed usage, omitted `usageDetails`, resolved
synchronously and overflowed the MB-to-bytes multiply for large values. Any
override of this surface can only add a tell, so the patch, the switch and its
renderer forwarding were removed.[^removal]

[^plan]: Build-pipeline plan - measured quota grows as 10 GiB + usage
[^p37]: Patch 0037 - kIncognitoStaticStorageQuota is on by default upstream since 154
[^removal]: Removal of the quota-override patch
