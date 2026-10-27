# Contributing to cuttle

## Check the recorded decisions first

cuttle keeps its design decisions, with the reasons and evidence behind them,
in [`docs/knowledge/`](docs/knowledge/index.md). Much of what looks like a bug
or a missing feature - a WebRTC policy, a canvas readback that differs, a font
that is not shipped, a flag cuttle refuses - is deliberate or a recorded known
gap. Before changing behaviour:

1. Read [the index](docs/knowledge/index.md) and every concept under
   [`docs/knowledge/decisions/`](docs/knowledge/decisions/) that touches the
   area you change, plus the findings it links.
2. If your change contradicts a decision, say so in the PR and explain what
   changed; update or replace the decision in the same PR. A concept that is no
   longer true is rewritten or deleted, never left beside the new behaviour.
3. If your change makes a new decision or establishes a finding that should
   outlive the PR, add a concept for it. The bundle follows the
   [Open Knowledge Format](https://github.com/GoogleCloudPlatform/open-knowledge-format):
   one Markdown file per concept with `type`, `description`, `generated` and
   `sources` in its frontmatter, and a line in the hand-written `index.md`.

Stealth changes need evidence, not a checker's score: the reasons are in
`packages/browser/README.md`. Fingerprint arg-building is pinned by a golden
(`packages/cuttle/internal/fingerprint/testdata/golden.json`); regenerate it
with `just parity-golden` only for a change you mean, and say why in the PR.

## Working on the code

- Run `lefthook install` once per clone, so `just check` and the secret scan
  run before each commit. `just check` is the whole gate, locally and in CI.
- PR titles are [Conventional Commits](https://www.conventionalcommits.org/)
  (`fix(serve): ...`). The PR body ends with a `## Release notes` section: one
  paragraph, at most 300 characters, written for someone running cuttle. CI
  checks both; [AGENTS.md](AGENTS.md#releasing) has the details.
- This is a public repository: no internal infrastructure, credentials or
  proprietary binaries, and no named commercial scraping targets.
- Keep changes small and the simplest thing that meets a real need today.
