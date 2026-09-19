---
okf_version: "0.2"
title: cuttle knowledge bundle
description: Durable decisions and findings behind cuttle's design, with rationale and evidence.
---

# cuttle knowledge bundle

Durable project knowledge: decisions with rationale, findings with evidence.
A concept belongs here only if it is true and useful in a fresh clone. State
(versions in flight, work status, counts) stays in git, PRs and the release
machinery; secrets never enter the repo; standing orders live in AGENTS.md
with at most a one-line pointer here, while the rationale lives in the
concept. This is a PUBLIC repository: no internal infrastructure references,
no named commercial targets, no credentials - the same non-negotiables as
AGENTS.md.

Type vocabulary: `Decision`, `Finding`, `Reference`, `Runbook`. Layout:
`decisions/`, `findings/`, `references/` (add `runbooks/` on first need).
This bundle is hand-indexed: when adding or changing a concept, update the
lists below (title, link, the concept's `description` verbatim).

## Decisions

- [Humanized input is the value proposition](decisions/humanize-over-speed.md) - Human-paced input is core to cuttle's stealth promise; decision-loop optimizations must never trade it away for action speed.
- [One driver at a time, enforced by a lease in the daemon](decisions/session-lease-in-the-daemon.md) - Exclusive driving of a browser is a TTL lease held in cuttle serve, keyed by seed, with explicit takeover - not a host file lock and not a second browser.
- [playwright-cli is the driver interface for higher-level automation](decisions/playwright-cli-is-the-driver-interface.md) - cuttle composes the bundled playwright-cli for all higher-level browsing automation (cuttle pw, cuttle jev-browse) - never raw CDP, never its own snapshot or ref semantics - and it is the only driver cuttle documents or routes agents to.
- [jev is a removable module](decisions/jev-is-a-removable-module.md) - Only internal/cli/jevbrowse.go (and its test) may import internal/jev, enforced by depguard, so the experimental jev-browse loop can be cut out at any time; cli code that needs aria parsing keeps its own small parser.

## Findings

- [playwright-cli attach and session model](findings/playwright-cli-attach-model.md) - How the bundled playwright-cli 0.1.20 decides to attach vs launch, where its session daemon keeps state, and the failure wordings the cuttle pw wrapper relies on.
- [Downloads API is seed-keyed; the reserved seed is unreachable in pool mode](findings/downloads-seed-keying.md) - GET /downloads resolves through a seed and rejects the reserved __default__ seed in pool mode, so driver-written files there must be asserted via exec, not the API.
- [The profile dir is the session's artifact store, not Chrome's scratch](findings/profile-dir-is-artifact-store.md) - A relaunch of the same seed must reuse its profile dir - downloads and driver outputs live there; only an ephemeral run may delete it.
- [TypeSafe Jev API transports and answer shapes](findings/jev-api-transports.md) - The Jev client routes one API key to OpenRouter or the first-party API by its prefix, and OpenRouter sends a confidence on choice answers despite documenting only probabilities.
- [The aria snapshot renders field values, password inputs included](findings/aria-snapshot-renders-secret-values.md) - playwright-cli's aria snapshot prints current field values in plaintext - type=password too, in several yaml shapes - so snapshot text must be filtered on the parsed tree before it leaves the host.
- [playwright-cli go-back leaves the snapshot emitting dead refs](findings/playwright-cli-go-back-ref-poisoning.md) - In the bundled playwright-cli 0.1.20, after go-back every snapshot ref is from the pre-navigation frame and clicks on it fail; only a fresh goto re-mints working refs.
- [Orchestrator reasoning effort is the largest browse-time lever](findings/orchestrator-effort-is-the-browse-time-lever.md) - On a humanized multi-page flow driven through cuttle pw by a headless claude -p orchestrator, reasoning effort moved wall time more than model choice or context size; most of a run is tool execution and turn overhead, not LLM time. On merged main, pw at low effort (~100-130s) is the fastest reliable configuration.
- [jev-browse slowness was the driver path, not the model](findings/jev-browse-slowness-was-the-driver-path.md) - jev-browse ran a multi-page flow at nearly twice plain cuttle pw's time because of how it drove the page - modal-blind offers, dead-click repeats, missing Enter, redundant snapshots and spawns, section-less labels - and reached parity with pw at high effort once those were fixed and merged.
- [A client-side app transition can update URL and title while the body stays hidden](findings/spa-transition-hides-body-behind-overlay.md) - On a signed-in site, a client-side transition from one app to another updates the URL and title while the body stays hidden behind a "Navigating to ..." overlay (innerText ~34 chars); the aria snapshot or a reload already holds the real page, so a done-check or an orchestrator that trusts URL+title or reads only innerText gives up on a page that was there.
- [Cost of one cuttle pw call](findings/pw-call-cost-breakdown.md) - A cuttle pw verb pays a flat ~250-320ms for docker exec, node start and the playwright-cli bundle load before the browser does anything, while cuttle's own wrapper cost 30-80ms until it shrank to one exec; a persistent in-container client removes most of the fixed cost at a maintenance price.
- [playwright-cli action verbs always write their snapshot to a file](findings/playwright-cli-action-verbs-snapshot-to-file.md) - In playwright-cli 0.1.20 every action verb writes the post-action aria snapshot to .playwright-cli/page-<ts>.yml and prints only a link; nothing configurable makes it print inline - only the snapshot verb does.
- [aria snapshot: focus and modal shape](findings/aria-snapshot-focus-and-modal-shape.md) - In playwright's aria snapshot [active] marks only the focused element and there is no [modal] marker, so an open modal is recognized as a dialog whose subtree holds [active]; background elements stay listed, and keys containing a colon-space are single-quoted whole.

## References

- [CLI surfaces of the open-source jev-browser projects](references/jev-browser-cli-surfaces.md) - How a dozen public decision-model browsing agents shape their command line - goal input, values, extraction, output, exit codes, session model, key handling - and which of those choices cuttle jev-browse took or refused.
