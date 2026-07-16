# Releasing cuttle

PR-merge-driven via [release-please](https://github.com/googleapis/release-please),
fully automated once the release PR is merged. The version and changelog come from
the conventional commits on `main`.

## Cutting a release

1. Land conventional commits on `main` (`feat:`, `fix:`, `chore:` ... - they
   become the changelog and drive the semver bump). release-please keeps a
   `chore(main): release X.Y.Z` PR open and up to date, bumping the version in
   `internal/cli/SKILL.md`'s frontmatter and `CHANGELOG.md`.
2. Run the gates: `go run ./test/smoke` against the built image, then the
   real-amd64 deployment check (`docs/UPGRADE.md`).
3. **Merge the release PR.** That is the release: release-please tags `vX.Y.Z` and
   opens the GitHub release, then the gated steps in the `release` job of
   `.github/workflows/ci.yml` publish (GoReleaser binaries + Homebrew cask, and
   the GHCR image) in the same run.

To force a specific version, add a commit with a `Release-As: X.Y.Z` footer before
merging; otherwise the version is derived automatically (non-breaking commits ->
patch, `feat!`/`fix!` -> minor pre-1.0).

## What the merge triggers (one `ci.yml` run)

- **release-please** - tags `vX.Y.Z`, opens the GitHub release, emits the
  `release_created` / `version` outputs the steps below gate on.
- **GoReleaser** (`.goreleaser.yaml`) - cross-builds the `cuttle` CLI
  (linux/darwin x amd64/arm64), appends the archives + checksums to the release,
  and pushes the Homebrew cask to `tenequm/homebrew-tap`.
- **image** - the linux/amd64 Docker image to `ghcr.io/glim-sh/cuttle` (tags
  `X.Y.Z`, `X.Y`, `latest`, `sha-...`).

## Install channels

- `brew install tenequm/tap/cuttle`
- `go install github.com/glim-sh/cuttle/cmd/cuttle@latest`
- `docker pull ghcr.io/glim-sh/cuttle:<version>`

## One-time setup (repo admin)

- **`GH_RELEASE_TOKEN` secret** on the repo: a fine-grained PAT with Contents
  read/write on `tenequm/homebrew-tap` (used only by GoReleaser to push the cask).
  release-please and the image push run on the default `github.token`.
- **Allow GitHub Actions to create PRs**: repo Settings -> Actions -> General ->
  "Allow GitHub Actions to create and approve pull requests" must be on, or
  release-please cannot open the release PR.
