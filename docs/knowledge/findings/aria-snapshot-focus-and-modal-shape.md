---
type: Finding
title: "aria snapshot: focus and modal shape"
description: In playwright's aria snapshot [active] marks only the focused element and there is no [modal] marker, so an open modal is recognized as a dialog whose subtree holds [active]; background elements stay listed, and keys containing a colon-space are single-quoted whole.
tags: [playwright-cli, snapshot, jev-browse, cuttle-pw]
status: stable
stale_after: "2027-03-19T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-04T22:00:00+00:00" }
sources:
  - id: renderer
    resource: "playwright-core bundled with @playwright/cli 0.1.20, re-checked at 0.1.23 (1.65.0-alpha-2026-10-30): the aria snapshot renderer and its yaml key escaping"
    title: playwright's aria snapshot renderer
  - id: live
    resource: "live validation against local cuttle containers, 2026-09-18/19 and 2026-11-04 (0.1.23): snapshots taken with an in-page modal, a native showModal dialog and an inert subtree open (session record, no durable link)"
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
- **Background elements stay listed.** Everything behind an in-page modal
  (a `role=dialog` element, `aria-modal` or not) is still in the tree with
  working-looking refs; clicking one times out on the driver's 5000ms
  actionability wait because the overlay intercepts it. A consumer offering
  elements must scope to the dialog itself. Behind a native `showModal()`
  dialog, and inside an `inert` subtree, the driver (since 0.1.23) lists the
  controls with no ref - `- button: behind` - so they cannot be offered or
  clicked at all.
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
