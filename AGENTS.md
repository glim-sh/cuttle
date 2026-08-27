# AGENTS.md - cuttle

cuttle is a browser for agents: `cuttle serve` is a CDP multiplexer that
spawns one stealth Chrome per fingerprint seed, routing per-seed identity
(fingerprint, proxy, geoip) over CDP. A single static Go binary with no Python
runtime dependency of its own. Note the runtime image is NOT Python-free:
`openbox`, installed for headed mode, pulls in `python3-minimal`. The build's
font-renaming stage is separate and does not ship.

## Layout

- `cmd/cuttle/` - CLI entrypoint. `internal/` - the packages (cli incl. the
  embedded SKILL.md, serve daemon, fingerprint arg-builder, backends, profile
  store, cdp, config). Go 1.26,
  gofumpt, golangci-lint v2, just. Module: `github.com/glim-sh/cuttle`.
- Run `lefthook install` when setting up a clone: `lefthook.yml` does nothing
  until its hooks are written into `.git/hooks`, so fmt, lint and the gitleaks
  secret scan silently do not run without it.
- `packages/browser/` - the self-hosted stealth-Chromium build pipeline: the
  patch series, the Linux build driver (`build/`), the Hetzner build-host scripts
  (`hetzner/`), and the behavioral validate harness (`validate/`). `versions.env`
  is the version/sha pin. See its README.
- `ops/docker/` - the container build assets: `Dockerfile` (stealth-Chromium
  runtime + headed Xvfb/openbox + KasmVNC; multi-arch, amd64 = Windows persona,
  arm64 = macOS persona), `bin/` (entrypoint + VNC viewer), `winfonts/README.md`
  (how the metric-compatible free font packs are built and renamed) and
  `macfonts/metrics.json` (the macOS metrics table those packs are stamped with).
  Build context is the repo root: `just build-image` (or `docker build -f
  ops/docker/Dockerfile .`). The build-context filter is
  `ops/docker/Dockerfile.dockerignore` (BuildKit's per-Dockerfile ignore file,
  takes precedence over any root `.dockerignore` - there is none here).
- `test/smoke/` - neutral, self-contained CDP smoke harness (`go run
  ./test/smoke` against a running container).
- `ops/helm/cuttle/` - Helm chart for the k8s backend.
- `docs/` - `OPERATING.md` (install, backends, ports, multi-profile mode,
  secrets, deployment - the operator half, kept deliberately OUT of the embedded
  SKILL.md so agents do not pay for it every session), `THIRD-PARTY.md`,
  `2608-18-improvements-issues-research/`, plus the kept post-mortem of the
  removed macOS backend.

## Non-negotiables

- KISS and YAGNI. Build the simplest thing that satisfies a real, present need;
  do not add config knobs, abstractions, backends, or interface seams for a
  hypothetical future caller. A second implementation earns an abstraction; one
  does not. Prefer deleting code to generalizing it. If a new file, flag, or
  indirection cannot name the concrete need it serves today, drop it.
- This is a PUBLIC repo. Never add internal infra references (clusters, k8s
  namespaces, proxies, secrets), named commercial scraping targets or
  "bypass X on <site>" framing, or any credential.
- No proprietary binaries: only the free stealth-Chromium fork (clark MIT). The
  daemon and fingerprint code are authored Go, not vendored from any licensed
  browser product.
- Stealth output is the whole game: fingerprint arg-building, proxy
  normalization, and geoip are snapshotted in the golden
  `internal/fingerprint/testdata/golden.json` (regenerate with `just
  parity-golden`). The golden is a regression tripwire - it turns any change to
  that output into a diff someone must consciously regenerate and review, so a
  stealth drift can never land silently. (It was originally captured
  byte-for-byte from the now-removed Python oracle.)
- Conventional Commits (`type(scope): description`). Everything the release
  process needs follows from the title and the `## Release notes` section of a
  PR body; see "Releasing" below.

## Releasing

Releasing is not a task. Land conventional commits on `main` and release-please
keeps a `chore(main): release X.Y.Z` PR open and current; merging that PR is the
release, and it publishes the binaries, the Homebrew cask and the GHCR image in
one `ci.yml` run. Nothing here is a judgement call, and none of it is manual:

- **Write the `## Release notes` section** at the bottom of the PR description.
  It becomes the changelog entry, verbatim, under that commit's bullet. One
  paragraph, at most 300 characters, no bullets and no blank lines, and the last
  section of the body - CI enforces every one of those and comments on the PR
  with all the failures at once. Write it for someone running cuttle: what
  changed for them and what they must do. Reviewer detail goes in the sections
  above it, which never reach the changelog. A release-wide lead or an
  `**Upgrading:**` block goes after a lone `[release-note]` line, exempt from the
  cap.
- **Never pick a version, never hand-write or hand-edit the release PR.**
  release-please derives the version from the commit types and regenerates that
  PR on every push to `main`, discarding anything written into it. It also
  decides on its own whether a release is warranted at all - a batch of purely
  internal commits opens no PR, and that is correct, not a fault to work around.
- **Merge the release PR however you like** - the button, `gh pr merge`, any
  subject. Its payload is `CHANGELOG.md` and the version bumps, which are files
  in the diff and land regardless of the commit message.
- **Fixing a note after merge:** edit the merged PR's description. Generation
  re-reads it (`ops/scripts/release-note-from-pr.sh`), so the correction lands in
  the next regeneration. Editing `CHANGELOG.md` by hand does nothing - it is
  regenerated from commits every time.
- **Preview locally, any time:** `git-cliff --config .github/cliff.toml
  --unreleased --tag vX.Y.Z`. Read-only, nothing to revert. The release PR also
  carries the same rendering as a sticky comment, and its `CHANGELOG.md` diff is
  the real thing - its *description* is release-please's own plainer rendering
  and must be left alone, since it parses that body on merge to decide what to
  release.
- **The squash settings are load-bearing.** The changelog prose is the PR body,
  which is only true while the repo squashes with `PR_TITLE`/`PR_BODY`. Verify
  and restore:

  ```bash
  gh api repos/glim-sh/cuttle --jq '{title: .squash_merge_commit_title, message: .squash_merge_commit_message}'
  gh api -X PATCH repos/glim-sh/cuttle -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY
  ```

Who owns what, when changing any of it: release-please computes the version,
opens the release PR, bumps every version-bearing file and cuts the tag
(`.github/release-please-config.json`); git-cliff renders `CHANGELOG.md` and the
GitHub release body (`.github/cliff.toml`); GoReleaser builds and appends the
artifacts (`ops/config/goreleaser.yaml`). release-please runs with
`skip-changelog`, so it and git-cliff never write the same file. Three gates
enforce what used to be documented: the PR title and note (`release-note.yml`),
the `word(` shape that makes release-please silently drop a commit (same
workflow), and the two halves of a version-bearing file
(`ops/scripts/check-version-files.sh`).
