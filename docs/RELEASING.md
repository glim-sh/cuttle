# Releasing cuttle

PR-merge-driven via [release-please](https://github.com/googleapis/release-please),
fully automated once the release PR is merged. The version and changelog come from
the conventional commits on `main`.

## Which commit types cut a release

Read this before reasoning about versions or picking a commit type. release-please
releases only **user-facing** types. Every other type is `hidden` in its default
changelog sections, so it renders an empty changelog and release-please skips the
release PR entirely, logging `No user facing commits found ... - skipping`.

| Commit | Releases? | Bump (pre-1.0, i.e. today) |
| --- | --- | --- |
| `fix:` | yes | patch |
| `feat:` | yes | patch (`bump-patch-for-minor-pre-major`) |
| `perf:`, `revert:` | yes | patch |
| any type with `!` or a `BREAKING CHANGE:` footer | yes | minor (`bump-minor-pre-major`) |
| `refactor:` `chore:` `docs:` `test:` `ci:` `build:` `style:` | **no - nothing happens** | n/a |

A hidden type does not produce a patch release. It produces **nothing**: no release
PR, no tag, no binaries, no image. `ci.yml` gates the GHCR image push on
`release_created`, so `ghcr.io/glim-sh/cuttle:latest` also stays exactly where it
was, and a CLI built from `main` keeps pulling the previous image.

The `!` is the whole difference: release-please keeps a commit that is
`isBreaking && hidden`, so `refactor!:` releases where `refactor:` is silently
dropped. Same diff, same type, opposite outcome.

Note `feat:` -> **patch** pre-1.0, not minor: `bump-patch-for-minor-pre-major`
remaps it, and `bump-minor-pre-major` remaps breaking changes down to minor. Both
revert to the standard SemVer mapping (feat -> minor, breaking -> major) at 1.0.

## The skip is a signal, not an obstacle

If you find yourself wanting a `refactor:` or `chore:` to release, the commit is
almost certainly mistyped. Hidden types encode a claim - *"nothing here for a
consumer"* - and the skip is that claim being enforced. Wanting it to ship is
evidence that something user-facing is in there after all.

Retype it by its **effect on the consumer**, not by which files moved:

- Changes what a consumer receives (env var names, CLI flags, the image command,
  `cuttle skill` output, CDP behavior)? That is `feat:` or `fix:` - plus `!` if it
  breaks them.
- A true refactor (identical shipped behavior)? Then there is nothing to deliver.
  Let it ride along with the next `feat:`/`fix:`; waiting costs nothing.

**Do not "fix" this by adding hidden types to `changelog-sections`.** The gate is a
correctness canary: it is what catches a breaking change wearing a `refactor:`
label before it ships as a quiet patch. Making refactors releasable is right for
repos whose *source is the artifact* (terraform modules, header-only libraries,
anything vendored by path) - consumers there read the source directly, so an
"internal" change is user-visible by construction. cuttle ships compiled binaries
and an image; a true refactor delivers a functionally identical artifact, so there
is genuinely nothing to release.

Escape hatches, if you are ever actually stuck: `!` for a breaking change of any
type, or a `Release-As: X.Y.Z` footer to force a specific version.

One cuttle-specific trap: `internal/cli/SKILL.md` is `//go:embed`ed into the binary
(`internal/cli/skill.go`) and printed by `cuttle skill`, so a real change to it IS
shipped behavior - type those `feat(skill):` / `fix(skill):`, not `docs:`. A
`docs:` change to `README.md` ships nothing and correctly releases nothing.

## Cutting a release

1. Land releasing conventional commits on `main` (see the table above). They become
   the changelog and drive the semver bump. release-please keeps a
   `chore(main): release X.Y.Z` PR open and up to date, bumping the version in
   `internal/cli/SKILL.md`'s frontmatter and `CHANGELOG.md`. If no release PR
   appears, re-read the table - you probably landed only hidden types.
2. Run the gates: `go run ./test/smoke` against the built image, then the
   real-amd64 deployment check (`docs/UPGRADE.md`).
3. **Merge the release PR.** That is the release: release-please tags `vX.Y.Z` and
   opens the GitHub release, then the gated steps in the `release` job of
   `.github/workflows/ci.yml` publish (GoReleaser binaries + Homebrew cask, and
   the GHCR image) in the same run.

Merging a feature PR never releases anything by itself - it only updates the
pending release PR. Merging *that* is the release.

### Squash-merge gotcha

The repo's squash title mode is `COMMIT_OR_PR_TITLE`. On a **single-commit PR**
GitHub prefills the squash subject from the *commit* title, not the PR title - so
retitling the PR in the UI does not change what release-please parses off `main`.
A PR titled `feat!:` whose lone commit says `feat:` lands as `feat:`. Either amend
the commit or set the subject explicitly at merge time:

```bash
gh pr merge <N> --squash --subject "feat!: ..." --body "..."
```

## Version-bearing files

Any file that names the released version must be bumped by release-please, never
by hand. Two things are required, and one without the other silently does nothing:

1. An `x-release-please-version` comment on the line holding the version.
2. The file listed under `extra-files` in `.github/release-please-config.json`.

Currently: `internal/cli/SKILL.md` (frontmatter), `ops/helm/cuttle/values.yaml`
(`image.tag`), `ops/helm/cuttle/Chart.yaml` (`appVersion`). `CHANGELOG.md` and
`.github/.release-please-manifest.json` are handled by release-please itself.

`Chart.yaml`'s own `version` is the chart's shape, not the app's - it is bumped by
hand and must NOT carry the annotation.

### List YAML/JSON/TOML files as `{"type": "generic"}`, never a bare string

For a bare `"path.yaml"` string, release-please runs *two* updaters: the
annotation-based `Generic` one you want, **and** a jsonpath updater hardcoded to
`$.version`. The second one reserializes the whole document through a YAML
dumper, which by design "removes all comments" - including the
`x-release-please-version` annotation the first updater needs. It runs first, so
the annotation is already gone by the time `Generic` looks for it.

On a file with a top-level `version:` key the result is the exact inverse of the
intent: the wrong line gets bumped, the annotated line is left stale, and every
comment in the file is stripped. `Chart.yaml` shipped that way in the 0.5.0
release PR - chart `version` 0.1.0 -> 0.5.0, `appVersion` frozen at 0.4.0.

The object form dispatches to `Generic` alone and is comment-safe:

```json
"extra-files": [
  { "type": "generic", "path": "ops/helm/cuttle/Chart.yaml" }
]
```

A file with no top-level `version:` key (like `values.yaml`) survives the bare
string only by accident - the jsonpath matches nothing, so it returns the content
untouched. Do not rely on that; list every YAML/JSON/TOML extra-file in the object
form. Markdown (`SKILL.md`) has no such updater and is fine as a bare string.

Adding a new file that embeds the version without doing both steps is how
`values.yaml` drifted to two releases stale (pinning a dead image and
CrashLoopBackOff-ing the k8s backend) without anything failing.

## What the merge triggers (one `ci.yml` run)

- **release-please** - tags `vX.Y.Z`, opens the GitHub release, emits the
  `release_created` / `version` outputs the steps below gate on.
- **GoReleaser** (`ops/config/goreleaser.yaml`) - cross-builds the `cuttle` CLI
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
