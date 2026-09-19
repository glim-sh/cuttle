---
type: Finding
title: "aria snapshot: focus and modal shape"
description: In playwright's aria snapshot [active] marks only the focused element and there is no [modal] marker, so an open modal is recognized as a dialog whose subtree holds [active]; background elements stay listed, and keys containing a colon-space are single-quoted whole.
tags: [playwright-cli, snapshot, jev-browse, cuttle-pw]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-19T11:30:00+01:00" }
sources:
  - id: renderer
    resource: "playwright-core bundled with @playwright/cli 0.1.20: the aria snapshot renderer and its yaml key escaping"
    title: playwright's aria snapshot renderer
  - id: live
    resource: "live validation against local cuttle containers, 2026-09-18/19: snapshots taken with an in-page modal open (session record, no durable link)"
    title: Live validation
---

# Finding

Shape facts anything parsing the aria snapshot has to handle:[^renderer][^live]

- **`[active]` marks only the focused element** - it is set when the node
  is `document.activeElement` and `document.hasFocus()` is true. It is not
  a "this subtree is live" marker.
- **There is no `[modal]` marker.** An open modal shows as a `dialog` or
  `alertdialog` node; the working test for "a modal is open" is such a node
  whose subtree contains `[active]`.
- **Background elements stay listed.** Everything behind the modal is still
  in the tree with working-looking refs; clicking one times out on the
  driver's 5000ms actionability wait because the overlay intercepts it.
  A consumer offering elements must scope to the dialog itself.
- **Keys containing ": " are single-quoted whole**, with `''` for a literal
  quote:

  ```yaml
  - 'dialog "Step 1: Sign in" [ref=e4]':
  ```

The value-rendering side of the same quoting rules is in
[the aria snapshot renders field values](/findings/aria-snapshot-renders-secret-values.md).
jev-browse's modal scoping and `cuttle pw`'s dialog hint on a click timeout
both rest on these facts - see
[jev-browse slowness was the driver path](/findings/jev-browse-slowness-was-the-driver-path.md).

[^renderer]: playwright's aria snapshot renderer
[^live]: Live validation
