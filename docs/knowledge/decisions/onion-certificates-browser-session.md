---
type: Decision
title: Onion certificate exceptions belong to a browser-level session
description: Opt-in onion TLS exceptions use one daemon-owned browser Security session per seed, independent of client connections.
status: stable
generated: { by: codex/gpt-6, at: "2026-11-07T02:39:00Z" }
sources:
  - id: implementation
    resource: ../../../packages/cuttle/internal/serve/onioncert.go
  - id: test
    resource: ../../../packages/cuttle/internal/serve/onioncert_e2e_test.go
  - id: chromium
    resource: https://github.com/chromium/chromium/blob/156.0.8078.47/content/browser/devtools/devtools_instrumentation.cc
  - id: security
    resource: https://github.com/chromium/chromium/blob/156.0.8078.47/content/browser/devtools/protocol/security_handler.cc
---

# Browser-level certificate handling

The opt-in serve setting installs `Security.enable` and
`Security.setOverrideCertificateErrors` on a dedicated browser connection,
then auto-attaches pages before publishing the seed. The browser-level handler
checks the failing request URL, never the page URL: only HTTPS/WSS hosts with
an explicit `.onion` suffix continue. All other errors are cancelled.[^implementation]
This accepts all certificate errors for onion hosts and assumes trusted Tor
routing; it does not supply that routing.

Chromium dispatches certificate errors to browser-level Security handlers
when a page agent exists. Auto-attachment forces those agents to exist for
future pages without waiting for a daemon-side per-page setup. The deprecated
Security commands still have implementations in the pinned source.[^chromium][^security]
A bundled-browser test covers default-off rejection, immediate navigation,
popups, subsequent tabs, lookalike hosts, and clearnet redirects and resources.
Popups need `Runtime.runIfWaitingForDebugger` even when their attachment event
reports `waitingForDebugger=false`.[^test]

This avoids patching Chromium or injecting a handler into every driver session.
It leaves humanization and client CDP IDs untouched. Installation failure
fails the seed launch; disconnection restores ordinary certificate checks.
Chrome's startup/restored navigations can precede CDP readiness and may need
a reload. Privileged clients can still explicitly request global certificate
ignoring; this setting is not a sandbox against those clients.[^implementation]

[^implementation]: [Daemon policy](../../../packages/cuttle/internal/serve/onioncert.go).
[^test]: [Bundled-browser regression](../../../packages/cuttle/internal/serve/onioncert_e2e_test.go).
[^chromium]: [Chromium certificate-error dispatch](https://github.com/chromium/chromium/blob/156.0.8078.47/content/browser/devtools/devtools_instrumentation.cc).
[^security]: [Chromium Security handler](https://github.com/chromium/chromium/blob/156.0.8078.47/content/browser/devtools/protocol/security_handler.cc).
