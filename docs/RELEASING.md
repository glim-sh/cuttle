# Releasing cuttle

PR-merge-driven via [release-please](https://github.com/googleapis/release-please),
fully automated once the release PR is merged. The version comes from conventional
commits; the changelog stays git-cliff (`.github/cliff.toml`), mirroring pond's format.

## Cutting a release

1. Land conventional commits on `main` (`feat:`, `fix:`, `chore:` ... - they
   become the changelog and drive the semver bump). release-please keeps a
   `chore(main): release X.Y.Z` PR open and up to date, bumping `pyproject.toml`
   and `SKILL.md`'s frontmatter version. Preview the pending changelog with
   `just release-preview`.
2. Run the gates: `just smoke`, then the real-amd64 deployment check
   (`docs/UPGRADE.md`). CI has no test gate by design.
3. **Merge the release PR.** That is the release: release-please tags `vX.Y.Z`
   and opens the GitHub release, then the gated jobs in `release.yml` publish.

To force a specific version (e.g. a minor bump the commits would not derive), add
a commit with a `Release-As: X.Y.Z` footer before merging; otherwise the version
is derived automatically (non-breaking commits -> patch, `feat!`/`fix!` -> minor
pre-1.0).

## What the merge triggers (`.github/workflows/release.yml`, one run)

- `release-please` - tags `vX.Y.Z`, opens the GitHub release, emits the outputs
  the jobs below gate on (`release_created`, `version`, `tag_name`)
- `dist` - `uv build` (sdist + wheel)
- `pypi` - `uv publish` via PyPI trusted publishing (OIDC, no token)
- `image` - linux/amd64 Docker image to `ghcr.io/glim-sh/cuttle` (tags
  `X.Y.Z`, `X.Y`, `latest`, `sha-...`)
- `finalize` - puts git-cliff notes + dist assets on the release, and commits the
  regenerated `CHANGELOG.md` and `uv.lock` back to `main` (release-please skips
  both: `skip-changelog`, and it has no uv support)
- `homebrew` - renders `Formula/cuttle.rb` from `uv.lock` (pinned sdist
  resources) and pushes it to `tenequm/homebrew-tap`; the formula fetches the
  sdist from the GitHub release

The nix flake (`flake.nix`) builds from source at any rev, so it needs no
per-release step.

## Install channels

- `uv tool install cuttle-browser` (or `pip install cuttle-browser`)
- `brew install tenequm/tap/cuttle`
- `nix run github:glim-sh/cuttle`
- `docker pull ghcr.io/glim-sh/cuttle:<version>`

## One-time setup (repo admin)

- **PyPI trusted publisher**: on pypi.org, add a pending publisher for project
  `cuttle-browser`: owner `glim-sh`, repo `cuttle`, workflow `release.yml`,
  environment `pypi`. No API token needed; the `pypi` GitHub environment is
  auto-created on first run.
- **`GH_RELEASE_TOKEN` secret** on `glim-sh/cuttle`: fine-grained PAT with
  Contents read/write on `tenequm/homebrew-tap` (used only by the `homebrew`
  job). release-please itself runs on the default `github.token`.
- **Allow GitHub Actions to create PRs**: repo Settings -> Actions -> General ->
  "Allow GitHub Actions to create and approve pull requests" must be on, or
  release-please cannot open the release PR.
- Optional: `nix flake lock` once and commit `flake.lock` to pin nixpkgs for
  flake consumers.
