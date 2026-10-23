---
type: Finding
title: A Chromium bump breaks the build host's source sync in three ways
description: On the 154 -> 156 bump the recovery gclient sync failed three ways - the depot_tools pinned for 156 rejects the absolute gclient_gn_args_file that ungoogled's depot_tools.patch writes; 156's llvm-libclang GCS dep gates non_git_source per object, which a dep-level check skips; and the full-history fetch draws HTTP 429s from googlesource that can leave a dep with no FETCH_HEAD - and a warm tree is never upgraded in place, so a bump needs a fresh clone.
tags: [build, chromium, depot_tools, gclient, upgrade]
status: stable
stale_after: "2027-04-23T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-10-23T16:12:55+00:00" }
sources:
  - id: host
    resource: "Build-host observation on 2026-10-23: the 154.0.8037.57 -> 156.0.8078.47 rebuild on a ccx63 restored from the warm-cache snapshot (sync logs, gclient errors, sync wall time)"
    title: 156 rebuild on the build host
  - id: gnargs
    resource: "https://github.com/glim-sh/cuttle/pull/129 (fix(browser): keep gclient_gn_args_file relative for depot_tools 156), /packages/browser/build/build-linux.sh Stage 2b"
    title: gclient_gn_args_file fix
  - id: ucpatch
    resource: https://github.com/ungoogled-software/ungoogled-chromium/blob/master/utils/depot_tools.patch
    title: ungoogled-chromium utils/depot_tools.patch - rewrites gclient_gn_args_file
  - id: gcs
    resource: "https://github.com/glim-sh/cuttle/pull/129 (fix(browser): fetch gcs deps gated per object, like 156's llvm-libclang), /packages/browser/build/build-linux.sh non_git_source step"
    title: Per-object GCS gating fix
  - id: readme
    resource: /packages/browser/README.md
    title: Build pipeline README - Version-bump sync gotchas and upgrade workflow
---

# A Chromium bump breaks the build host's source sync in three ways

**A warm tree is not upgraded in place.** `build-linux.sh` clones Chromium only
into an empty `build/src`, so a bump on the warm snapshot needs `build/` and
`ungoogled-chromium/` moved aside and a fresh clone. The recovery sync fetched
full git history (v8 alone was 4+ GB) and a fresh sync took about 40 min; with
`--no-history` (as clone.py itself syncs) it took 17 min, v8's `.git` 39 MB.[^host]

**depot_tools rejects an absolute `gclient_gn_args_file`.** ungoogled's
depot_tools patch rewrites DEPS' `src/...` value to an absolute output
path.[^ucpatch] The depot_tools pinned for 156 refuses that with "Must be a
relative path without '..' traversal", so every sync failed. The fix drops the
rewrite and resolves the relative name from the checkout parent.[^gnargs]

**A GCS dep can gate `non_git_source` per object.** 156 added
`third_party/llvm-libclang` (`libclang.so` plus python bindings) with the
condition `host_os == "linux" and non_git_source` on each object rather than on
the dep. Our step that fetches `non_git_source` deps checked only the dep, skipped
it, and the build stopped at the pre-build missing-input check.[^host] Objects
are now filtered by their own condition.[^gcs]

**googlesource rate-limits the parallel full-history fetch.** Under the 48-way
parallel fetch googlesource returned HTTP 429. A 429 mid-fetch can leave a dep
with no `FETCH_HEAD`, and every later attempt then fails at
`git reset --hard FETCH_HEAD`, so the sync's own retries cannot recover it.
Checking the dep out at its DEPS-pinned commit (which the moved-aside old tree
often already holds) and re-running the fetch once does.[^host] The procedure
lives in the README.[^readme]

[^host]: 156 rebuild on the build host
[^ucpatch]: ungoogled-chromium utils/depot_tools.patch - rewrites gclient_gn_args_file
[^gnargs]: gclient_gn_args_file fix
[^gcs]: Per-object GCS gating fix
[^readme]: Build pipeline README - Version-bump sync gotchas and upgrade workflow
