---
type: Finding
title: The aria snapshot renders field values, password inputs included
description: playwright-cli's aria snapshot prints current field values in plaintext - type=password too - so snapshot text must have value suffixes stripped before it leaves the host.
tags: [secrets, playwright-cli, jev-browse, snapshot]
status: stable
generated: { by: claude-code/claude-opus-5, at: "2026-09-18T15:55:00+00:00" }
sources:
  - id: code
    resource: /packages/cuttle/internal/jev/loop.go
    title: pageLines and valueRE (regression-tested in loop_test.go)
  - id: live
    resource: "live validation against a local cuttle image build, 2026-09-18: a filled password field's value appeared in the snapshot (session record, no durable link)"
    title: Live validation
---

# Finding

The driver's aria snapshot renders the current value of an input as the
node line's suffix - `- textbox "Password" [ref=e8]: hunter2` - and does so
for `type=password` fields as well. After a `{{cuttle:NAME}}` fill, that
value IS the substituted secret.[^live][^code]

Consequence: any feature that ships snapshot-derived text off the host
(`cuttle jev-browse --extract`, any future summarizer) must drop value
suffixes first, or a secret the sentinel system kept out of the transcript
leaves the machine anyway.

How jev-browse holds this line:

- `pageLines`, the only path that sends page text to the API, skips every
  node whose role carries a value (`textbox`, `searchbox`, `combobox`,
  `spinbutton`, `slider`) via `valueRE`; a regression test pins it.[^code]
- The decide path was names-only by design from the start: the model sees
  value NAMES, and the value itself is looked up locally only after the
  answer returns.[^code]

Anyone adding a new consumer of snapshot text should reuse that filtering,
not re-derive it.

Related: [playwright-cli is the driver interface](/decisions/playwright-cli-is-the-driver-interface.md).

[^code]: pageLines and valueRE (regression-tested in loop_test.go)
[^live]: Live validation
