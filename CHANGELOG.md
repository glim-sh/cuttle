# Changelog

## [0.6.0](https://github.com/glim-sh/cuttle/compare/v0.5.3...v0.6.0) (2026-07-21)


### ⚠ BREAKING CHANGES

* the native macOS backend and the `cuttle mcp` command are removed; `cuttle view` is now an alias of `cuttle connect` (no window-raise).

### Features

* remove native macOS backend and `cuttle mcp` ([3270393](https://github.com/glim-sh/cuttle/commit/32703931ffcafd68efe9668ea9f9df2f7e813db7))

## [0.5.3](https://github.com/glim-sh/cuttle/compare/v0.5.2...v0.5.3) (2026-07-17)


### Features

* **backend:** native macOS backend (local, no Docker/VNC) ([97a5b99](https://github.com/glim-sh/cuttle/commit/97a5b9936faaec2b6233b972ac0c1d1c9f421ae2))

## [0.5.2](https://github.com/glim-sh/cuttle/compare/v0.5.1...v0.5.2) (2026-07-17)


### Bug Fixes

* **serve:** stop killing cold-starting Chrome on readiness-poll disconnect ([541af9c](https://github.com/glim-sh/cuttle/commit/541af9c0339208f54f9b3fea06a2c2a81d6ffdcc))

## [0.5.1](https://github.com/glim-sh/cuttle/compare/v0.5.0...v0.5.1) (2026-07-17)


### Bug Fixes

* **cli:** resolve go install version at use-site, not in init ([47a23db](https://github.com/glim-sh/cuttle/commit/47a23db1d85139245ddd76d47567c454b997dbc2))

## [0.5.0](https://github.com/glim-sh/cuttle/compare/v0.4.0...v0.5.0) (2026-07-17)


### ⚠ BREAKING CHANGES

* the container's env var contract is renamed. CUTTLESERVE_PROXY -> CUTTLE_PROXY, CUTTLESERVE_HOST -> CUTTLE_HOST, CUTTLESERVE_EPHEMERAL -> CUTTLE_EPHEMERAL, CLOAKSERVE_IDLE_TIMEOUT -> CUTTLE_IDLE_TIMEOUT, and CLOAKBROWSER_BINARY_PATH -> CUTTLE_BROWSER_BINARY. The old names are no longer read. The /usr/local/bin/cuttleserve shim is removed; the image command is now `cuttle serve`.

### Bug Fixes

* **release:** stop release-please bumping the wrong version in Chart.yaml ([#8](https://github.com/glim-sh/cuttle/issues/8)) ([867fd87](https://github.com/glim-sh/cuttle/commit/867fd87e91dd7a07690d8fb74cd2527d6ae3cbbd))


### Code Refactoring

* rename runtime env vars to CUTTLE_*; drop cuttleserve shim ([#6](https://github.com/glim-sh/cuttle/issues/6)) ([014dedb](https://github.com/glim-sh/cuttle/commit/014dedb4441e5e7f91140f6f1622243edc38ea9f))

## [0.4.0](https://github.com/glim-sh/cuttle/compare/v0.3.0...v0.4.0) (2026-07-16)


### ⚠ BREAKING CHANGES

* the Python package and all Python-based distribution are removed; cuttle now ships as a Go binary (`go install` / Homebrew cask) plus the `ghcr.io/glim-sh/cuttle` image. The Go module is `github.com/glim-sh/cuttle`, and the bare-metal serve data dir moved to `$XDG_DATA_HOME/cuttle/serve` (was `~/.cloakbrowser/cloakserve`).

### Features

* rewrite cuttle in Go; remote backends + local-canonical profiles ([6987473](https://github.com/glim-sh/cuttle/commit/6987473bd2e8bf12d31a8f22f8aea1d54cbdb899))

## [0.3.0](https://github.com/glim-sh/cuttle/releases/tag/v0.3.0) - 2026-07-13

### <!-- 2 -->🐛 Bug Fixes
- **packaging:** split container-only deps so brew/pip/nix install cleanly ([4f2e197](https://github.com/glim-sh/cuttle/commit/4f2e197b04c3d565e9df96bf0866b54f91e2facf))

### <!-- 4 -->🚜 Refactor
- **cli:** dedup docker inspect, tighten status/comments ([7eb2df7](https://github.com/glim-sh/cuttle/commit/7eb2df7bcc7d86e311a1dbab54cb8fc8e23648dd))

### <!-- 5 -->📚 Documentation
- **release:** document release-please bump semantics + pre-1.0 bump flags ([6f506c9](https://github.com/glim-sh/cuttle/commit/6f506c9b13f91656ae1babaabd693578f638796f))

### <!-- 6 -->🧹 Chores
- adopt release-please for PR-merge-driven releases ([8d221e3](https://github.com/glim-sh/cuttle/commit/8d221e30e4871460e81256e7bfdab5854cab44d8))
- remove accidentally-committed .playwright-cli session artifacts ([96cb857](https://github.com/glim-sh/cuttle/commit/96cb8575ab119d1918814169fad151df8d1827ee))
- move release tooling config under .github/ ([354872c](https://github.com/glim-sh/cuttle/commit/354872c01947176e1b0e36629d9c0dcb6fc4452a))
- **main:** release 0.3.0 ([c3d81e1](https://github.com/glim-sh/cuttle/commit/c3d81e12767a4feef0f21c1112870a284020bfc7))
- add lint + type-check workflow for PRs and main ([84401b7](https://github.com/glim-sh/cuttle/commit/84401b7b801ad930eb558dda3893ce9288b2c688))
- bump all workflow actions to latest major versions ([a75268b](https://github.com/glim-sh/cuttle/commit/a75268b97a04ec72736b6298fcbf85d02c251bcc))
- pin setup-uv to v8.3.2 (no moving v8 major tag published) ([2e58c30](https://github.com/glim-sh/cuttle/commit/2e58c30b6cc33e78ba48adb60293b3b702079a9a))
- add path-filtered smoke workflow (build + harness over CDP) ([9603db4](https://github.com/glim-sh/cuttle/commit/9603db4e45dbe8b0e14fc97070bdc07d3ccf76e9))

### <!-- 7 -->🔧 Other
- Merge branch 'main' into release-please--branches--main--components--cuttle-browser ([1ba6b62](https://github.com/glim-sh/cuttle/commit/1ba6b622487d64f33e7746473824f746ec58450f))
- Merge pull request #2 from glim-sh/release-please--branches--main--components--cuttle-browser

chore(main): release 0.3.0 ([176a7d5](https://github.com/glim-sh/cuttle/commit/176a7d5c1da5d7732da6b202cef66d8aa1e70bca))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.2.0...v0.3.0

## [0.2.0](https://github.com/glim-sh/cuttle/releases/tag/v0.2.0) - 2026-07-10

### <!-- 1 -->🎉 New Features
- host CLI + VNC login-handoff, vendor/ restructure, amd64-only engine ([a7f78c6](https://github.com/glim-sh/cuttle/commit/a7f78c66a6afe263bcafafc706d7140eed931fb8))
- bundle SKILL.md into the package; publish as cuttle-browser ([aceaa92](https://github.com/glim-sh/cuttle/commit/aceaa92d2e40579b4d21fca8dd25852c73830905))
- **cli:** live driver briefing from up/status; SKILL.md becomes policy-only ([ce74655](https://github.com/glim-sh/cuttle/commit/ce74655fcd26d1b42a865a6a1fdd0f106b1c984a))
- **release:** tag-driven publishing - PyPI, GHCR, GitHub release, homebrew tap, nix flake ([3d9a6de](https://github.com/glim-sh/cuttle/commit/3d9a6de52e71ad039712c01c42f4adf2401154de))

### <!-- 2 -->🐛 Bug Fixes
- **cli:** self-heal zombie containers; add `cuttle skill` ([000aa79](https://github.com/glim-sh/cuttle/commit/000aa7942ddcc21ff73b76fefcb88f57ed9deb2b))
- **cli:** strip driver's self-echoed name from version line ([f701023](https://github.com/glim-sh/cuttle/commit/f70102342c91d612f426010c5c414ead6cb7056b))

### <!-- 4 -->🚜 Refactor
- move VNC viewer page out of root into bin/ ([2741220](https://github.com/glim-sh/cuttle/commit/27412207ae20f4f78315aeb8f08fbc4b339939f6))

### <!-- 5 -->📚 Documentation
- link stealth-verification guide from README ([fa22b25](https://github.com/glim-sh/cuttle/commit/fa22b25c1c713ee831ff33ac617ef2e8cffdd7d8))
- install via PyPI cuttle-browser / uvx; add README CLI section ([abf2e00](https://github.com/glim-sh/cuttle/commit/abf2e001ecc645a57cb6e7006ff871b1c5387f79))
- **cli:** clarify driver fallback wording in briefing + SKILL.md ([92556bd](https://github.com/glim-sh/cuttle/commit/92556bd87457be66082a89b43945a96f6a66b7b8))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.1.0...v0.2.0

## [0.1.0](https://github.com/glim-sh/cuttle/releases/tag/v0.1.0) - 2026-07-09

### <!-- 1 -->🎉 New Features
- cuttle - stealth-Chromium CDP farm ([b7c7d71](https://github.com/glim-sh/cuttle/commit/b7c7d71c80fcd67ba7e2e3b1caf4f79955d4ab23))

### <!-- 2 -->🐛 Bug Fixes
- **cuttleserve:** bind 0.0.0.0 under k8s/containerd, not just docker/podman ([f20a15d](https://github.com/glim-sh/cuttle/commit/f20a15df5a0b68542dc869444058b165e29c1ec6))

### <!-- 4 -->🚜 Refactor
- consolidate utility scripts into scripts/, vendor doc into docs/ ([ce0bdf7](https://github.com/glim-sh/cuttle/commit/ce0bdf7f1e7bc00647a26471b49f5319c15a4ea1))

### <!-- 5 -->📚 Documentation
- add stealth-identity verification guide ([4603600](https://github.com/glim-sh/cuttle/commit/46036003385037b27f293451fe0eca34c7dcbb09))

### <!-- 6 -->🧹 Chores
- simplify quickstart, drop redundant NOTICE, fold python pin into pyproject ([0d824b3](https://github.com/glim-sh/cuttle/commit/0d824b39abc3b1ee9f679c8c24e146861224f1b6))
