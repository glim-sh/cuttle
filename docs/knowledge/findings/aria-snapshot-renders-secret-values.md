---
type: Finding
title: The aria snapshot renders field values, password inputs included
description: playwright-cli's aria snapshot prints current field values in plaintext - type=password too, in several yaml shapes - so snapshot text must be filtered on the parsed tree before it leaves the host.
tags: [secrets, playwright-cli, jev-browse, snapshot]
status: stable
generated: { by: claude-code/claude-opus-5, at: "2026-09-18T23:10:00+00:00" }
sources:
  - id: code
    resource: /packages/cuttle/internal/jev/loop.go
    title: pageLines and holdsValue (regression-tested in loop_test.go)
  - id: renderer
    resource: "playwright-core bundled with @playwright/cli 0.1.20: packages/isomorphic/ariaSnapshotRenderer.ts and yaml.ts (yamlEscapeKeyIfNeeded, yamlEscapeValueIfNeeded)"
    title: playwright's aria snapshot renderer
  - id: live
    resource: "live validation against local cuttle image builds, 2026-09-18: a filled password field's value appeared in the snapshot, and a field labelled with \": \" slipped a line-regex filter (session record, no durable link)"
    title: Live validation
---

# Finding

The driver's aria snapshot renders the current value of an input as the
node line's suffix - `- textbox "Password" [ref=e8]: hunter2` - and does so
for `type=password` fields as well. After a `{{cuttle:NAME}}` fill, that
value IS the substituted secret.[^live][^code]

The value is not always a plain suffix. Playwright's renderer single-quotes
the whole node key (`''` escaping a quote) whenever it would not read back
as a yaml key - most often a label holding ": " - double-quotes a value that
needs it, and when the node also has a property such as a placeholder it
prints the value as a child `- text:` line instead:[^renderer][^live]

```yaml
- 'textbox "Password: required" [ref=e8]': hunter2
- textbox "Secret" [ref=e9]:
  - /placeholder: Enter it
  - text: hunter2
```

Consequence: any feature that ships snapshot-derived text off the host
(`cuttle jev-browse --extract`, any future summarizer) must filter the
parsed tree, not match lines - a regex anchored on the role read the quoted
form as page text and sent the password. The sections around the tree carry
their own leaks: `### Open tabs` lists every tab's full URL, query string
included.

How jev-browse holds this line:

- `pageLines`, the only path that sends page text to the API, reads only the
  nodes of the `### Snapshot` section as `parseLine` unquoted them, and
  drops every node whose role carries a value (`textbox`, `searchbox`,
  `combobox`, `spinbutton`, `slider`) together with everything nested under
  it; regression tests pin each shape above.[^code]
- A node line the parser cannot read is kept as opaque and skipped with its
  subtree, so a parse gap drops text instead of sending it.[^code]
- Residual limit: playwright renders a value for any `<input>`/`<textarea>`,
  whatever its role, so a field a page gives another explicit role (gridcell,
  option, ...) prints its value as that role's text, and a bare
  contenteditable reads as ordinary text. jev only fills textbox, searchbox
  and combobox roles, so its own `{{cuttle:NAME}}` fills are covered; values a
  page or a person put into such fields are not.[^renderer]
- The decide path was names-only by design from the start: the model sees
  value NAMES, and the value itself is looked up locally only after the
  answer returns.[^code]

Anyone adding a new consumer of snapshot text should reuse that filtering,
not re-derive it.

Related: [playwright-cli is the driver interface](/decisions/playwright-cli-is-the-driver-interface.md).

[^code]: pageLines and holdsValue (regression-tested in loop_test.go)
[^renderer]: playwright's aria snapshot renderer
[^live]: Live validation
