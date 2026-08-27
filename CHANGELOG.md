# Changelog

## [0.14.1](https://github.com/glim-sh/cuttle/compare/v0.14.0...v0.14.1) - 2026-08-27

### <!-- 2 -->🐛 Bug Fixes
- **ci:** drop the blank a filtered footer left, and say when the PAT is missing ([7815dab](https://github.com/glim-sh/cuttle/commit/7815dab7898b89855bf521dc70dd67472a031599))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.14.0...v0.14.1

## [0.14.0](https://github.com/glim-sh/cuttle/compare/v0.13.1...v0.14.0) - 2026-08-27

### <!-- 1 -->🎉 New Features
- **serve:** daemon-owned secret injection, capture and masking ([#56](https://github.com/glim-sh/cuttle/pull/56)) ([3abb156](https://github.com/glim-sh/cuttle/commit/3abb156075d398fe5820e7a7d37ab7f64724be3a))
  An agent can now drive a login without the credential entering its
  context. `cuttle secret set` hands a value to the session from a vault,
  a prompt or the page itself, a driver fills it by name as
  `{{cuttle:NAME}}`, and cuttle masks it in every line it writes.

### <!-- 2 -->🐛 Bug Fixes
- **ci:** the version-files gate scanned build output and a symlink ([32c5727](https://github.com/glim-sh/cuttle/commit/32c57274f82b30ecb32a8b33c7fff468bd77854b))
- **ci:** push the changelog as a real user, not as the bot ([3483be7](https://github.com/glim-sh/cuttle/commit/3483be72fdfec38c2815905810ee0353a7390adb))

### <!-- 4 -->🚜 Refactor
- **ci:** regenerate the changelog whole, and preview it on the release PR ([eb064f9](https://github.com/glim-sh/cuttle/commit/eb064f9305fd4574f39f0ea112bf15c5ac8a035c))

### <!-- 5 -->📚 Documentation
- **researches:** do not reproduce a real credential in a public repo ([53dab7d](https://github.com/glim-sh/cuttle/commit/53dab7df76af8ecad7bfc1682e6bec47fd36a563))
- note that a clone needs lefthook install ([92861e5](https://github.com/glim-sh/cuttle/commit/92861e5d2994dcb5c152b1e038be8a7de8f4bfbf))

### <!-- 6 -->🧹 Chores
- scan staged changes for secrets with gitleaks ([2e47412](https://github.com/glim-sh/cuttle/commit/2e47412cf1b6812ae4cd9f01ee79232599234a28))
- **release:** generate the changelog with git-cliff, and gate what a doc used to ([61065ce](https://github.com/glim-sh/cuttle/commit/61065ce61f4069edbec0fa3c2b21ee6f9eb18731))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.13.1...v0.14.0

## [0.13.1](https://github.com/glim-sh/cuttle/compare/v0.13.0...v0.13.1) - 2026-08-25

### <!-- 1 -->🎉 New Features
- **serve:** match stock Chrome on third-party cookies, add opt-out ([#54](https://github.com/glim-sh/cuttle/pull/54)) ([bc33f66](https://github.com/glim-sh/cuttle/commit/bc33f66459b15a77bb649cd24313d09c9d18d240))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.13.0...v0.13.1

## [0.13.0](https://github.com/glim-sh/cuttle/compare/v0.12.0...v0.13.0) - 2026-08-20

### <!-- 0 -->🛠 Breaking Changes
- **serve:** [**breaking**] session mode by default - one browser per container ([4b96ab2](https://github.com/glim-sh/cuttle/commit/4b96ab27bc551c987e6febd22e5372d9a43e6a28))

### <!-- 5 -->📚 Documentation
- reframe cuttle as a browser for agents, not a farm ([f7f0321](https://github.com/glim-sh/cuttle/commit/f7f03214a3009c22ab0844d45eb5a164f35340f9))
- **readme:** add why-not-Claude-in-Chrome section ([049445d](https://github.com/glim-sh/cuttle/commit/049445dc670eaa1ec6c5cf8242889cfd31637ee8))
- **readme:** replace comparison list with a table, add ChatGPT column ([08e12dd](https://github.com/glim-sh/cuttle/commit/08e12ddb1030c2fe1ffd7ad4c7d479cbe607360a))

### <!-- 6 -->🧹 Chores
- build images with Docker's github-builder reusable workflow ([46727af](https://github.com/glim-sh/cuttle/commit/46727afb176caceb01de991e445fb3a1da30bfb7))
- drop farm wording from help text, cask, chart and image metadata ([3b32978](https://github.com/glim-sh/cuttle/commit/3b32978e9298989790b2e70cfcc3302105e91b09))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.12.0...v0.13.0

## [0.12.0](https://github.com/glim-sh/cuttle/compare/v0.11.5...v0.12.0) - 2026-08-19

### <!-- 0 -->🛠 Breaking Changes
- **browser:** [**breaking**] self-hosted stealth-chromium 151, measured against real hardware ([55549f5](https://github.com/glim-sh/cuttle/commit/55549f5567d1d6e8884ce40e255e7284e4c0996c))

### <!-- 5 -->📚 Documentation
- **browser:** size the build cache volume deliberately, and how to move it ([a168f5a](https://github.com/glim-sh/cuttle/commit/a168f5a5a0bc3207de1f9328f70b2c29ee3222c9))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.11.5...v0.12.0

## [0.11.5](https://github.com/glim-sh/cuttle/compare/v0.11.4...v0.11.5) - 2026-08-18

### <!-- 2 -->🐛 Bug Fixes
- support playwright-cli 0.1.18 and close six agent-facing defects ([97d69e2](https://github.com/glim-sh/cuttle/commit/97d69e290ed9eeb20e358db726a61b469d3681ce))

### <!-- 5 -->📚 Documentation
- **researches:** add agent-experience issues and improvements research ([a6e4bfe](https://github.com/glim-sh/cuttle/commit/a6e4bfe2c24635fe233acf0acb3c16e8f3231863))
- **researches:** add code deltas, critics, session summaries; move to a research dir ([a19c2a2](https://github.com/glim-sh/cuttle/commit/a19c2a2183631a073ca29537a2b69214eb4cb3f8))

### <!-- 6 -->🧹 Chores
- bump Go toolchain to 1.26.6 and setup-qemu-action to v4 ([54aeff2](https://github.com/glim-sh/cuttle/commit/54aeff2d51e522ec7d134cae2c8552b9ee139450))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.11.4...v0.11.5

## [0.11.4](https://github.com/glim-sh/cuttle/compare/v0.11.3...v0.11.4) - 2026-08-08

### <!-- 1 -->🎉 New Features
- **serve:** keep background tabs interactive and type fills as keystrokes ([9284508](https://github.com/glim-sh/cuttle/commit/92845083cd3e8deb82da159ec48c0650c0424805))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.11.3...v0.11.4

## [0.11.3](https://github.com/glim-sh/cuttle/compare/v0.11.2...v0.11.3) - 2026-08-08

### <!-- 2 -->🐛 Bug Fixes
- **cdp:** stop the daemon panicking when a localStorage read hits its deadline ([4bb58a2](https://github.com/glim-sh/cuttle/commit/4bb58a25ceb59b7a02901b4cf044f17e5a112a65))

### <!-- 5 -->📚 Documentation
- correct the 0.11.2 changelog ([e8f28c2](https://github.com/glim-sh/cuttle/commit/e8f28c215043210ada08a9fbe666b5e621009b9a))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.11.2...v0.11.3

## [0.11.2](https://github.com/glim-sh/cuttle/compare/v0.11.1...v0.11.2) - 2026-08-07

### <!-- 2 -->🐛 Bug Fixes
- **fingerprint:** give the macOS persona a Mac display, CPU and memory ([e6d55dd](https://github.com/glim-sh/cuttle/commit/e6d55ddfb58d0ca476d2ccaf82e9315dd7993412))
- **fingerprint:** coherent Apple machines, and stop advertising a broken WebGPU ([1dfb719](https://github.com/glim-sh/cuttle/commit/1dfb719778fdd3378ebed5afd1dd477a3dac842e))

### <!-- 7 -->🔧 Other
- **fingerprint:** keep WebGPU enabled; a null adapter is a documented pass ([6245326](https://github.com/glim-sh/cuttle/commit/6245326e248532b75af6342b75bcdcecd4b4ec3d))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.11.1...v0.11.2

## [0.11.1](https://github.com/glim-sh/cuttle/compare/v0.11.0...v0.11.1) - 2026-08-07

### <!-- 1 -->🎉 New Features
- **serve:** --allow-context-creation opt-out for drivers that must create contexts ([25112de](https://github.com/glim-sh/cuttle/commit/25112deca8aab597841cc6e7b777095c1f41ad50))
- **fingerprint:** pin each seed's screen and size its window to match ([a94cbf2](https://github.com/glim-sh/cuttle/commit/a94cbf2bf972ce763b8249bb09bb93353fe0fee1))
- **cli:** surface --allow-context-creation on cuttle up and the chart ([8f7a812](https://github.com/glim-sh/cuttle/commit/8f7a8129ded31cb27f49641733164a46721206a6))

### <!-- 2 -->🐛 Bug Fixes
- **serve:** keep a created context on the seed's identity and in its snapshot ([c60a367](https://github.com/glim-sh/cuttle/commit/c60a36764728a930dbb3a33441211117e5097e1b))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.11.0...v0.11.1

## [0.11.0](https://github.com/glim-sh/cuttle/compare/v0.10.3...v0.11.0) - 2026-07-25

### <!-- 0 -->🛠 Breaking Changes
- **browser:** [**breaking**] self-hosted stealth-Chromium build pipeline ([eafb74e](https://github.com/glim-sh/cuttle/commit/eafb74e257dd6828047f0ce5988bb3c8b2598a29))

### <!-- 5 -->📚 Documentation
- record the release-please commit-body parse trap ([84e71d6](https://github.com/glim-sh/cuttle/commit/84e71d6e1e4d1e307dafc188eab972d43e1e2d5e))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.10.3...v0.11.0

## [0.10.3](https://github.com/glim-sh/cuttle/compare/v0.10.2...v0.10.3) - 2026-07-24

### <!-- 1 -->🎉 New Features
- tunnel auto-reconnect supervisor, click-humanizer parity, and container zombie reaping ([478ec7c](https://github.com/glim-sh/cuttle/commit/478ec7c0b1a35ca24000d91ad40ff12791f9c15c))

### <!-- 2 -->🐛 Bug Fixes
- self-heal the default browser and stop the viewer going black ([e1bfdd2](https://github.com/glim-sh/cuttle/commit/e1bfdd25ae5af835e4674cd5a2192204662d9ec7))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.10.2...v0.10.3

## [0.10.2](https://github.com/glim-sh/cuttle/compare/v0.10.1...v0.10.2) - 2026-07-24

### <!-- 1 -->🎉 New Features
- container downloads, logs verb, and port auto-discovery ([8db50fe](https://github.com/glim-sh/cuttle/commit/8db50fe0a084f825c0868864341a88db0f9aa73e))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.10.1...v0.10.2

## [0.10.1](https://github.com/glim-sh/cuttle/compare/v0.10.0...v0.10.1) - 2026-07-23

### <!-- 1 -->🎉 New Features
- **cli:** playwright-cli default; advise against mid-work teardown ([d6243d4](https://github.com/glim-sh/cuttle/commit/d6243d45af0530344412a4914811d1ab95e9eb38))

### <!-- 2 -->🐛 Bug Fixes
- **backend:** detect a host-port collision on local start ([e87b126](https://github.com/glim-sh/cuttle/commit/e87b126c7d57c6fd0f6dc40869d1bd0d5e7d2510))
- **cli:** don't warn '--image is fixed' when --recreate will apply it ([056c55e](https://github.com/glim-sh/cuttle/commit/056c55e7612b8d784b2cb2103b5b94ec64b86de6))
- **cli:** lighter, current, self-locating driver docs ([009884d](https://github.com/glim-sh/cuttle/commit/009884dcd74aef5a9486adcd330ea02db4d362a0))
- **cli:** portable playwright docs command instead of an absolute path ([d7e699e](https://github.com/glim-sh/cuttle/commit/d7e699eedf6844f12088cc573fbdc8608ef583d4))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.10.0...v0.10.1

## [0.10.0](https://github.com/glim-sh/cuttle/compare/v0.9.2...v0.10.0) - 2026-07-23

### <!-- 0 -->🛠 Breaking Changes
- [**breaking**] behavioral input humanization (on by default), keep-alive tab, capture telemetry ([#34](https://github.com/glim-sh/cuttle/pull/34)) ([ebb805d](https://github.com/glim-sh/cuttle/commit/ebb805d80cb58d3fbdeda1766d038cf41876fa3a))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.9.2...v0.10.0

## [0.9.2](https://github.com/glim-sh/cuttle/compare/v0.9.1...v0.9.2) - 2026-07-23

### <!-- 1 -->🎉 New Features
- **serve:** log Chrome exit cause instead of discarding it ([c53ab7a](https://github.com/glim-sh/cuttle/commit/c53ab7a3d67995981a8234de3f6ccb6efe97f749))

### <!-- 2 -->🐛 Bug Fixes
- **cdp:** detach tabs during state capture instead of closing them ([c98ab22](https://github.com/glim-sh/cuttle/commit/c98ab2289f7070766f106672a7ef5bef51f0ab88))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.9.1...v0.9.2

## [0.9.1](https://github.com/glim-sh/cuttle/compare/v0.9.0...v0.9.1) - 2026-07-22

### <!-- 2 -->🐛 Bug Fixes
- **k8s,cli:** CUTTLE_PORT crash, storageclass detection, k8s image pin, and cold-launch wait ([#30](https://github.com/glim-sh/cuttle/pull/30)) ([450a5e7](https://github.com/glim-sh/cuttle/commit/450a5e7f7bda32d3daef636b0e21d7ec4b46cbbc))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.9.0...v0.9.1

## [0.9.0](https://github.com/glim-sh/cuttle/compare/v0.8.3...v0.9.0) - 2026-07-22

### <!-- 0 -->🛠 Breaking Changes
- **profile:** [**breaking**] persist the default profile across recreate via a named volume/PVC; add --purge-profile ([#28](https://github.com/glim-sh/cuttle/pull/28)) ([427e5fa](https://github.com/glim-sh/cuttle/commit/427e5fa7b5239f0bdaffc7be432b818adac8fea1))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.8.3...v0.9.0

## [0.8.3](https://github.com/glim-sh/cuttle/compare/v0.8.2...v0.8.3) - 2026-07-22

### <!-- 2 -->🐛 Bug Fixes
- coherent default-seed timezone + eliminate Linux font-enumeration leak ([#25](https://github.com/glim-sh/cuttle/pull/25)) ([1f6c6ba](https://github.com/glim-sh/cuttle/commit/1f6c6ba6d02ee2b026970b629d418d518c158749))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.8.2...v0.8.3

## [0.8.2](https://github.com/glim-sh/cuttle/compare/v0.8.1...v0.8.2) - 2026-07-22

### <!-- 2 -->🐛 Bug Fixes
- **serve:** make auth-state checkpoint non-invasive so it can't corrupt live logins ([#21](https://github.com/glim-sh/cuttle/pull/21)) ([bdcf03c](https://github.com/glim-sh/cuttle/commit/bdcf03cbd1c324b7b802cc5a9c81fa02e5d90066))
- **fingerprint:** re-enable coherent referrers so same-origin POST Origin isn't null ([#22](https://github.com/glim-sh/cuttle/pull/22)) ([d39e850](https://github.com/glim-sh/cuttle/commit/d39e85047faa555230994930a7d5f1d676c8441c))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.8.1...v0.8.2

## [0.8.1](https://github.com/glim-sh/cuttle/compare/v0.8.0...v0.8.1) - 2026-07-21

### <!-- 2 -->🐛 Bug Fixes
- **release:** keep generated Homebrew cask brew-style-clean ([a5010b7](https://github.com/glim-sh/cuttle/commit/a5010b7fa5468cf4bd4fdf5fb4e4ba4fd3d28421))

### <!-- 5 -->📚 Documentation
- **cli:** fix stale --profile help; open no longer checks out state ([dc5441d](https://github.com/glim-sh/cuttle/commit/dc5441de41e0115de967713bac99952475990ae0))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.8.0...v0.8.1

## [0.8.0](https://github.com/glim-sh/cuttle/compare/v0.7.0...v0.8.0) - 2026-07-21

### <!-- 0 -->🛠 Breaking Changes
- **cli:** [**breaking**] make `cuttle open` a dumb navigate; move profile sync to lifecycle edges ([#19](https://github.com/glim-sh/cuttle/pull/19)) ([406ec49](https://github.com/glim-sh/cuttle/commit/406ec49e6d74a63845b6a22de623b1e1ee28beb6))

### <!-- 2 -->🐛 Bug Fixes
- **vnc:** fix macOS Cmd shortcuts and harden viewer clipboard ([#17](https://github.com/glim-sh/cuttle/pull/17)) ([f164a48](https://github.com/glim-sh/cuttle/commit/f164a4889c4d799322cd32bb62c5efc171479985))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.7.0...v0.8.0

## [0.7.0](https://github.com/glim-sh/cuttle/compare/v0.6.0...v0.7.0) - 2026-07-21

### <!-- 0 -->🛠 Breaking Changes
- [**breaking**] CLI UX overhaul - idempotent backends, stable tunnels, leaner surface ([#14](https://github.com/glim-sh/cuttle/pull/14)) ([19850b5](https://github.com/glim-sh/cuttle/commit/19850b598d106a6eacf23a53234d9e1cbea2afb4))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.6.0...v0.7.0

## [0.6.0](https://github.com/glim-sh/cuttle/compare/v0.5.3...v0.6.0) - 2026-07-21

### <!-- 0 -->🛠 Breaking Changes
- [**breaking**] remove native macOS backend and `cuttle mcp` ([3270393](https://github.com/glim-sh/cuttle/commit/32703931ffcafd68efe9668ea9f9df2f7e813db7))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.5.3...v0.6.0

## [0.5.3](https://github.com/glim-sh/cuttle/compare/v0.5.2...v0.5.3) - 2026-07-17

### <!-- 1 -->🎉 New Features
- **backend:** native macOS backend (local, no Docker/VNC) ([97a5b99](https://github.com/glim-sh/cuttle/commit/97a5b9936faaec2b6233b972ac0c1d1c9f421ae2))

### <!-- 6 -->🧹 Chores
- **release:** author the release PR with a PAT so its CI runs without approval ([a5fda9f](https://github.com/glim-sh/cuttle/commit/a5fda9f58419788df17020ca3ed5eb50de05d388))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.5.2...v0.5.3

## [0.5.2](https://github.com/glim-sh/cuttle/compare/v0.5.1...v0.5.2) - 2026-07-17

### <!-- 2 -->🐛 Bug Fixes
- **serve:** stop killing cold-starting Chrome on readiness-poll disconnect ([541af9c](https://github.com/glim-sh/cuttle/commit/541af9c0339208f54f9b3fea06a2c2a81d6ffdcc))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.5.1...v0.5.2

## [0.5.1](https://github.com/glim-sh/cuttle/compare/v0.5.0...v0.5.1) - 2026-07-17

### <!-- 2 -->🐛 Bug Fixes
- **cli:** resolve go install version at use-site, not in init ([47a23db](https://github.com/glim-sh/cuttle/commit/47a23db1d85139245ddd76d47567c454b997dbc2))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.5.0...v0.5.1

## [0.5.0](https://github.com/glim-sh/cuttle/compare/v0.4.0...v0.5.0) - 2026-07-17

### <!-- 0 -->🛠 Breaking Changes
- [**breaking**] rename runtime env vars to CUTTLE_*; drop cuttleserve shim ([#6](https://github.com/glim-sh/cuttle/pull/6)) ([014dedb](https://github.com/glim-sh/cuttle/commit/014dedb4441e5e7f91140f6f1622243edc38ea9f))

### <!-- 2 -->🐛 Bug Fixes
- **release:** stop release-please bumping the wrong version in Chart.yaml ([#8](https://github.com/glim-sh/cuttle/pull/8)) ([867fd87](https://github.com/glim-sh/cuttle/commit/867fd87e91dd7a07690d8fb74cd2527d6ae3cbbd))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.4.0...v0.5.0

## [0.4.0](https://github.com/glim-sh/cuttle/compare/v0.3.0...v0.4.0) - 2026-07-16

### <!-- 0 -->🛠 Breaking Changes
- [**breaking**] rewrite cuttle in Go; remote backends + local-canonical profiles ([6987473](https://github.com/glim-sh/cuttle/commit/6987473bd2e8bf12d31a8f22f8aea1d54cbdb899))

### <!-- 5 -->📚 Documentation
- **plans:** add cuttle Go rewrite + remote backends plan ([aaac1b7](https://github.com/glim-sh/cuttle/commit/aaac1b7f63520ad9d604dcb44baca94b42cf1c62))

### <!-- 6 -->🧹 Chores
- **smoke:** don't fail on GHA cache flakes; skip release-please PRs ([7048fcb](https://github.com/glim-sh/cuttle/commit/7048fcbc4a779c47b5e4140104a4cbf5ade461e3))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.3.0...v0.4.0

## [0.3.0](https://github.com/glim-sh/cuttle/compare/v0.2.0...v0.3.0) - 2026-07-13

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
- add lint + type-check workflow for PRs and main ([84401b7](https://github.com/glim-sh/cuttle/commit/84401b7b801ad930eb558dda3893ce9288b2c688))
- bump all workflow actions to latest major versions ([a75268b](https://github.com/glim-sh/cuttle/commit/a75268b97a04ec72736b6298fcbf85d02c251bcc))
- pin setup-uv to v8.3.2 (no moving v8 major tag published) ([2e58c30](https://github.com/glim-sh/cuttle/commit/2e58c30b6cc33e78ba48adb60293b3b702079a9a))
- add path-filtered smoke workflow (build + harness over CDP) ([9603db4](https://github.com/glim-sh/cuttle/commit/9603db4e45dbe8b0e14fc97070bdc07d3ccf76e9))

### <!-- 7 -->🔧 Other
- Merge branch 'main' into release-please--branches--main--components--cuttle-browser ([1ba6b62](https://github.com/glim-sh/cuttle/commit/1ba6b622487d64f33e7746473824f746ec58450f))
- Merge pull request #2 from glim-sh/release-please--branches--main--components--cuttle-browser

chore(main): release 0.3.0 ([176a7d5](https://github.com/glim-sh/cuttle/commit/176a7d5c1da5d7732da6b202cef66d8aa1e70bca))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.2.0...v0.3.0

## [0.2.0](https://github.com/glim-sh/cuttle/compare/v0.1.0...v0.2.0) - 2026-07-10

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
