---
type: Finding
title: TypeSafe Jev API transports and answer shapes
description: The Jev client routes one API key to OpenRouter or the first-party API by its prefix, and OpenRouter sends a confidence on choice answers despite documenting only probabilities.
tags: [jev, jev-browse, api, openrouter]
status: stable
stale_after: "2027-03-18T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5, at: "2026-09-18T15:55:00+00:00" }
sources:
  - id: code
    resource: /packages/cuttle/internal/jev/client.go
    title: Jev client (with request goldens in internal/jev/testdata/)
  - id: live
    resource: "live call transcript against both transports, 2026-09-18 (session record, no durable link)"
    title: Live calls
  - id: or
    resource: https://openrouter.ai/typesafe/jev-1.13
    title: OpenRouter model page for typesafe/jev-1.13
---

# Finding

The TypeSafe Jev client reads one env var, `CUTTLE_TYPESAFE_API_KEY`, and
routes by the key's prefix: a key starting `sk-or-` goes to OpenRouter
(`POST https://openrouter.ai/api/alpha/decisions`), anything else to the
first-party `api.typesafe.ai`. There is no flag and no second env var - the
key says which it is.[^code]

- **Request bodies are identical except the model line**, pinned by two
  goldens (`request.golden.json`, `request.openrouter.golden.json`). On
  OpenRouter the model is pinned to `typesafe/jev-1.13`, never the moving
  `~typesafe/jev-latest` alias: a model that changes under a fixed prompt
  changes every decision without a line of diff.[^code][^or]
- **OpenRouter does return `confidence` on choice answers**, although its
  documented answer shape describes only `probabilities`. The two differ:
  a pick with probability 0.99 came back with confidence 0.98. The client
  prefers the server-sent confidence and derives it from
  `probabilities[choice]` only when absent.[^live][^code]
- **A noul answer has no confidence field** on either transport - its
  probability is its confidence - matching the first-party docs, so the
  loop holds nouls to the same threshold as choices.[^live][^code]
- Observed cost profile: roughly 485-734ms per decision step and about
  $0.00006 per step at $0.042/M input tokens, output free.[^live][^or]

The API is days old at capture; re-check answer shapes, pricing and the
pinned model's availability before relying on them past `stale_after`.

Related: [Humanized input is the value proposition](/decisions/humanize-over-speed.md).

[^code]: Jev client (with request goldens in internal/jev/testdata/)
[^live]: Live calls
[^or]: OpenRouter model page for typesafe/jev-1.13
