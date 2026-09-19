---
type: Finding
title: playwright-cli action verbs always write their snapshot to a file
description: In playwright-cli 0.1.20 every action verb writes the post-action aria snapshot to .playwright-cli/page-<ts>.yml and prints only a link; nothing configurable makes it print inline - only the snapshot verb does.
tags: [playwright-cli, cuttle-pw, snapshot]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T11:30:00+01:00" }
sources:
  - id: core
    resource: "playwright-core as installed by @playwright/cli 0.1.20 in the image: the snapshot response path (snapshotToFile) and resolveCLIConfigForCLI's daemon overrides"
    title: playwright-core cli source at the pinned version
---

# Finding

After an action verb (click, fill, type, goto, ...), playwright-cli 0.1.20
writes the page's aria snapshot to `.playwright-cli/page-<timestamp>.yml`
in the working directory and prints only a link to it, never the tree:[^core]

```js
snapshotToFile = includeSnapshot !== "explicit" || !!fileName
```

and `resolveCLIConfigForCLI` hard-codes `snapshotMode: "full"` in the daemon
overrides, which are merged last. So no config file and no environment
variable can make an action verb print its snapshot inline. Only the
`snapshot` verb prints inline.[^core]

Consequence: a consumer that wants the tree after an action either reads
the linked file (inside the container, where the driver runs) or issues a
`snapshot` verb - one more call. Re-verify on every driver pin bump.

Related: [playwright-cli attach and session model](/findings/playwright-cli-attach-model.md).

[^core]: playwright-core cli source at the pinned version
