# Releasing cuttle

Tag-driven, fully automated after the tag. Version and changelog come from
conventional commits (git-cliff), mirroring pond's release format.

## Cutting a release

1. Land conventional commits on `main` (`feat:`, `fix:`, `chore:` ... - they
   become the changelog and drive the semver bump).
2. Run the gates: `just smoke`, then the real-amd64 deployment check
   (`docs/UPGRADE.md`). CI has no test gate by design.
3. `just release` - computes the next version from commits since the last tag
   (`uvx git-cliff --bumped-version`), sets it in `pyproject.toml`/`uv.lock`
   (`uv version`), regenerates `CHANGELOG.md`, commits, tags `vX.Y.Z`, pushes.
   Pass an explicit version to override: `just release 0.3.0`.

The tag push triggers `.github/workflows/release.yml`:

- `dist` - `uv build` (sdist + wheel)
- `pypi` - `uv publish` via PyPI trusted publishing (OIDC, no token)
- `image` - linux/amd64 Docker image to `ghcr.io/glim-sh/cuttle`
- `github-release` - GitHub release with git-cliff notes + dist artifacts
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
  Contents read/write on `tenequm/homebrew-tap` (same convention as pond).
- Optional: `nix flake lock` once and commit `flake.lock` to pin nixpkgs for
  flake consumers.
