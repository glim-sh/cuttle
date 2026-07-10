# Changelog

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
