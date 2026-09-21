# Changelog

## [0.15.1](https://github.com/glim-sh/cuttle/compare/v0.15.0...v0.15.1) - 2026-09-21

### <!-- 2 -->🐛 Bug Fixes
- **jev:** a none never implies done, and a fill is never refused as a write ([#118](https://github.com/glim-sh/cuttle/pull/118)) ([a3981a5](https://github.com/glim-sh/cuttle/commit/a3981a55b73af9266133662d2c9db178ce7705a8))
  `cuttle jev-browse` no longer ends done on a `none` - the model finding
  nothing to do never implies the task is finished, and the run exits 3 at
  the confidence the model gave. Typing into a box is never refused as a
  write, so a search the task asks for is no longer refused before it
  runs.
- **fingerprint:** disable the back/forward cache so refs survive go-back ([#121](https://github.com/glim-sh/cuttle/pull/121)) ([cf69edd](https://github.com/glim-sh/cuttle/commit/cf69edd933e94c3114a1436a2cd700e3c96339e3))
  cuttle now launches Chrome with the back/forward cache disabled, so
  playwright-cli refs survive `go-back`: snapshot after it and click as on
  any other page. The jev-browse re-goto after `back` and the SKILL.md
  advice to `goto` instead of `go-back` are gone; nothing to change on
  your side.

### <!-- 5 -->📚 Documentation
- **kb:** stress-round verdict, the jev decision layer and two playwright-cli driver facts ([#119](https://github.com/glim-sh/cuttle/pull/119)) ([69bfed1](https://github.com/glim-sh/cuttle/commit/69bfed1c152a49b6bed07c284d6d0b58e44e81f9))
  The knowledge base in docs/knowledge gained the 2026-09-19 stress-round
  verdict, the jev-browse decision-layer rationale with its live
  calibration evidence, and two playwright-cli driver facts (goto before
  hydration, find output). Documentation only - nothing changes for anyone
  running cuttle.

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.15.0...v0.15.1

## [0.15.0](https://github.com/glim-sh/cuttle/compare/v0.14.2...v0.15.0) - 2026-09-21

### <!-- 1 -->🎉 New Features
- bundle playwright-cli into the image with a cuttle pw passthrough ([#67](https://github.com/glim-sh/cuttle/pull/67)) ([28d9846](https://github.com/glim-sh/cuttle/commit/28d98463f9ac4338693bf0f1c7f4ac06ce8297d9))
  The image now bundles playwright-cli 0.1.20, version-locked to the
  browser it
  ships with. `cuttle pw <args...>` (alias for `cuttle playwright-cli`)
  runs it
  inside the container on every backend; it can only attach to cuttle's
  browser,
  and its `--filename` output comes back via `cuttle downloads`.
- **cli:** zero-ceremony cuttle pw - auto-attach on demand, allow open ([a7e6491](https://github.com/glim-sh/cuttle/commit/a7e649198ebcf68f46469120e3d1bd827dc7b0bb))
- **cli:** cuttle jev-browse - autonomous browsing loop on the bundled driver ([#66](https://github.com/glim-sh/cuttle/pull/66)) ([0496957](https://github.com/glim-sh/cuttle/commit/04969577150b91f3c9cb4599a8fa93e037206dda))
  New `cuttle jev-browse --task "..."` drives the browser toward a goal
  itself: a fast decision model picks each next element and the bundled
  playwright-cli acts, in the `cuttle pw` session. Set
  CUTTLE_TYPESAFE_API_KEY (TypeSafe or OpenRouter key) or pass --mock;
  finish blocked runs with `cuttle pw`.

  ---------

- **cli:** colored jev-browse output ([#70](https://github.com/glim-sh/cuttle/pull/70)) ([cf6f109](https://github.com/glim-sh/cuttle/commit/cf6f1092b66a7275e835a396965a17b7ba5e729d))
  `cuttle jev-browse` now prints colored, easier-to-scan progress on a
  terminal: faint step metadata, bold actions, a highlighted low
  confidence, a marked done/blocked/failed outcome and a standout handoff
  command. It honors NO_COLOR and stays plain text when piped.
- **cli:** make --context/--name global so pw and jev-browse reach any instance ([#74](https://github.com/glim-sh/cuttle/pull/74)) ([9df5c33](https://github.com/glim-sh/cuttle/commit/9df5c33a3f98af5f957ba5731dba888d88d398e6))
  `--context` and `--name` are now global flags, so `cuttle --name <name>
  pw ...` and `cuttle jev-browse --name <name>` drive a non-default
  instance. `CUTTLE_NAME` and a per-context `name` config key select it
  too. With `cuttle pw`, put them first. The `up` briefing now names the
  instance.
- **serve:** session lease - one driver at a time, with takeover ([#69](https://github.com/glim-sh/cuttle/pull/69)) ([87724c8](https://github.com/glim-sh/cuttle/commit/87724c8c82bd3c7718a7f2d2af487cd2c4bb077e))
  `cuttle jev-browse` now holds the browser while it runs: a second run,
  or a `cuttle pw` verb that drives the page, is refused naming who holds
  it; read verbs still work. Pass `--takeover` (for `cuttle pw`, before
  the verb) to take the browser over; the evicted run stops with exit 1.
- **cli:** center the skill and briefing on cuttle pw, drop host driver routing ([#75](https://github.com/glim-sh/cuttle/pull/75)) ([33eca16](https://github.com/glim-sh/cuttle/commit/33eca168a12b253c301d58d49e429910b9c6adab))
  `cuttle up` now points only at the bundled `cuttle pw` driver - no host
  driver detection or install hints. `cuttle pw --help` also lists the
  driver's own verbs. jev-browse is marked experimental, and its handoff
  command keeps your `--name`/`--context`.
- **cli:** deliver pw action snapshots as a host file, secrets masked ([#103](https://github.com/glim-sh/cuttle/pull/103)) ([78adbbb](https://github.com/glim-sh/cuttle/commit/78adbbb7c81d12196e5c926441479572aba420ed))
  `cuttle pw` action verbs now save their page snapshot on this host under
  `~/.local/state/cuttle/<instance>/snapshots/` and print that path, so it
  can be read directly without another `snapshot` call. Values held by
  `cuttle secret` appear as `{{cuttle:NAME}}` in the saved file.
- **jev:** withhold write actions and halve jev-browse run time ([#102](https://github.com/glim-sh/cuttle/pull/102)) ([9ca9386](https://github.com/glim-sh/cuttle/commit/9ca9386163a7478c150a2503b44162b9084c264d))
  `cuttle jev-browse` never offers write-shaped controls (send, post,
  apply, save, follow, message, pay, ...) and lists them as withheld,
  labels each option with its page section and state, and drives verbs
  through one persistent client - roughly twice as fast on real navigation
  flows.
- **serve:** mask password field values in /snapshot ([#109](https://github.com/glim-sh/cuttle/pull/109)) ([2a352b5](https://github.com/glim-sh/cuttle/commit/2a352b52c0d0eccd677a74ac3323e3cb38b3394c))
  The `[Snapshot]` file `cuttle pw` copies to the host now also masks the
  current value of every password field on the page as
  `{{cuttle:password-field}}`, so a password typed as a literal no longer
  lands on the host in the clear. Held secrets still show as
  `{{cuttle:NAME}}`. Nothing to configure.

### <!-- 2 -->🐛 Bug Fixes
- **serve:** keep the default seed's downloads across browser relaunch ([aac8e40](https://github.com/glim-sh/cuttle/commit/aac8e402f4687ee3318e7b98555780ce26cd3765))
- **cli:** run --extract on every ending, not only done ([#71](https://github.com/glim-sh/cuttle/pull/71)) ([4954195](https://github.com/glim-sh/cuttle/commit/4954195595c80c2540556b55bda425180616a669))
  `cuttle jev-browse --extract` now runs against the page the run ended on
  for every outcome, not only when the task finished: a blocked or
  out-of-steps run prints its lines too. Exit codes are unchanged; an
  extract that fails there prints one note and keeps its code.
- **cli:** re-attach the driver after a restart leaves a stale session ([#77](https://github.com/glim-sh/cuttle/pull/77)) ([bbeb0f0](https://github.com/glim-sh/cuttle/commit/bbeb0f06bcbfb3c8a45c5b86c7e4ac5986069536))
  `cuttle pw` and `cuttle jev-browse` now re-attach on their own after a
  container is killed or restarted uncleanly, or the in-container driver
  dies, instead of failing every verb with a Node stack trace until a
  manual `cuttle pw attach`.
- **viewer:** keep websocket inside proxy prefix ([#62](https://github.com/glim-sh/cuttle/pull/62)) ([3b10ba7](https://github.com/glim-sh/cuttle/commit/3b10ba70f877a367af3b9f66ba84a0fe186cc602))
  Viewer WebSockets now remain under reverse-proxy path prefixes while
  direct viewers continue to use /websockify. Image smoke coverage
  verifies direct HTTP and prefixed HTTPS connections.
- **smoke:** never exec into a container the run was not pointed at ([#79](https://github.com/glim-sh/cuttle/pull/79)) ([9075244](https://github.com/glim-sh/cuttle/commit/9075244efa3c6142d134d1485b29d1876a319666))
  The smoke harness's driver checks now refuse to run unless `CUTTLE_NAME`
  names the container behind `CUTTLE_URL`, so a local smoke run can no
  longer kill or drive the browser in your own `cuttle` container.
- **cli:** never fall back to another instance when the selected one is stopped ([#78](https://github.com/glim-sh/cuttle/pull/78)) ([a1df361](https://github.com/glim-sh/cuttle/commit/a1df3616d2a925c642bb57ae048526094305ded4))
  Verbs on a stopped or absent named instance no longer fall back to the
  default ports and hit a different browser - they refuse by name with the
  command that resumes it. `cuttle --name X up` now restarts, reuses and
  recreates a container on the ports it was created with instead of
  failing on 9222.
- **cli:** close the lifecycle gaps a stress run left open ([#88](https://github.com/glim-sh/cuttle/pull/88)) ([899a535](https://github.com/glim-sh/cuttle/commit/899a535658c0a5bb6df06232151fd9686ea2f3c6))
  `cuttle pw --takeover` now works in any order with `--name`/`--context`.
  Against an image older than the bundled driver, the briefing and
  `pw`/`jev-browse` say to run `up --recreate`. `up --recreate` names an
  image change. Verbs right after a restart wait for the daemon.
- **jev:** read quoted snapshot lines and keep field values out of --extract ([#89](https://github.com/glim-sh/cuttle/pull/89)) ([09111a2](https://github.com/glim-sh/cuttle/commit/09111a269c03bc4b62b0412a021e6b0230eae24a))
  jev-browse now sees elements whose names contain ": " (PR titles,
  "Password: required" fields), and --extract never sends a filled field's
  value or open-tab URLs to the API, so a secret --text is safe to combine
  with --extract. --extract also picks far more list items.
- **serve:** dismiss a native dialog the driver leaves open ([#87](https://github.com/glim-sh/cuttle/pull/87)) ([14f24cb](https://github.com/glim-sh/cuttle/commit/14f24cbf3858485bef0fed7911194572de9f57f9))
  A native dialog left open when a `cuttle pw` session ends no longer
  freezes every later `cuttle pw` command. cuttle dismisses it. `pw fill
  ... -- --help` now respects the session lease, and parallel `cuttle pw`
  calls after a browser crash no longer fail while re-attaching.
- **cli:** close the gaps a review of the lifecycle fixes found ([#90](https://github.com/glim-sh/cuttle/pull/90)) ([c46aa5e](https://github.com/glim-sh/cuttle/commit/c46aa5ebaa308f5f7c05e1b56c345637d92867ff))
  An empty --name/--context or CUTTLE_NAME/CUTTLE_CONTEXT is refused
  instead of acting on the default instance. up --recreate pulls and
  checks ports before removing the old container. pw works right after a
  restart. open --until no longer reports a slow page as gone.
- **serve:** pool-mode reaping, capture tab, hung tabs and launch bursts ([#86](https://github.com/glim-sh/cuttle/pull/86)) ([3b42ea2](https://github.com/glim-sh/cuttle/commit/3b42ea2d1dfc2c1ea8033d3d7db8de978031e3ed))
  Pool mode: HTTP-only probes are now idle-reaped, reconnects no longer
  land on a closing capture tab, tabs no longer hang after
  create-and-navigate, launch bursts queue, and a seed relaunched mid-reap
  keeps its profile. `secret rm` of an unknown name and a negative `--ttl`
  now fail.
- **backend:** keep profile cleanup and remote argv from reaching past their own ([#92](https://github.com/glim-sh/cuttle/pull/92)) ([f52cfd3](https://github.com/glim-sh/cuttle/commit/f52cfd3f0e709acc64345905e98956b63f6e9ec8))
  Profile cleanup no longer force-removes a volume, which on podman could
  take a concurrent `up`'s container with it, and `cuttle pw` arguments
  containing braces or a leading `=` now reach the driver unchanged on ssh
  contexts.
- **serve:** skip an idle reap whose timer a launch has since re-armed ([#93](https://github.com/glim-sh/cuttle/pull/93)) ([c667868](https://github.com/glim-sh/cuttle/commit/c667868b31aacfa520702a967f4ae3f72e2ba881))
  Pool mode no longer kills a browser it has just handed to a client when
  an idle timeout fires at the same moment as a connect, and a relaunch of
  a crashed seed is no longer torn down right after it starts.
- **serve:** dismiss a dialog that opened while no client was attached ([#91](https://github.com/glim-sh/cuttle/pull/91)) ([10b060b](https://github.com/glim-sh/cuttle/commit/10b060b5d3dafd72410821737a50aa35cdca6b1f))
  A native dialog (alert, confirm, "Leave site?") that opens while no
  driver is attached no longer hangs the next `cuttle pw` command for 30s:
  cuttle dismisses it when the next client connects. A dialog that opens
  while a driver is attached still reaches the driver.
- **serve:** keep the dialog watch alive through a signal shutdown ([#94](https://github.com/glim-sh/cuttle/pull/94)) ([4f4f1d0](https://github.com/glim-sh/cuttle/commit/4f4f1d0b1f480ec1ea7a091e62c26506fab0ccea))
  `cuttle down` or a container stop no longer waits out its whole snapshot
  budget when a native dialog opened while no driver was attached. The
  dialog is dismissed before the final capture, so every seed's final
  state is saved.
- **cli:** name the in-page dialog behind a pw click timeout ([#99](https://github.com/glim-sh/cuttle/pull/99)) ([11beeeb](https://github.com/glim-sh/cuttle/commit/11beeebdbc117a9fcd043247b62f4fc6ff859ded))
  `cuttle pw` now explains a click, hover or other pointer action that
  times out behind an open in-page dialog: it adds one stderr line naming
  the dialog and the ref of a button that only closes it, else `press
  Escape`. Output and exit codes are otherwise unchanged.
- **jev:** judge done on the page that landed, and end done on a none above even odds ([#108](https://github.com/glim-sh/cuttle/pull/108)) ([a9cb5a8](https://github.com/glim-sh/cuttle/commit/a9cb5a8f3e6c247e68a36ddc6bac73d02c9f5a01))
  jev-browse no longer declares a task done from a page whose URL and
  title changed before its body did: a read that still shows the previous
  page's controls is re-read until the new page is there. A run that
  reaches its page and finds nothing left to do now exits 0 done instead
  of 3 blocked.
- **cli:** downloads --wait accepts a finished download, logs drops D-Bus spam ([#111](https://github.com/glim-sh/cuttle/pull/111)) ([dcc2f46](https://github.com/glim-sh/cuttle/commit/dcc2f4663ef5f4821da45f6acaff21a9fac08e97))
  `cuttle downloads --latest --wait 30s` now pulls a download that already
  finished within the last 30s instead of timing out when the download
  beat the pull; waiting for a new one is unchanged. `cuttle logs` drops
  Chrome's repeated D-Bus connection errors so the lines that matter stay
  readable.
- **pw:** snapshot links a host file like an action, and the console log line names its verb ([#110](https://github.com/glim-sh/cuttle/pull/110)) ([c902c58](https://github.com/glim-sh/cuttle/commit/c902c582cf7227f8eb78737bd8719d33ec2d9cf3))
  `cuttle pw snapshot` now saves the masked snapshot to a host file like
  click/goto do and prints its path plus the first 40 lines; `--raw` keeps
  the whole tree inline. The console log line points at `cuttle pw
  console` instead of a container path, and snapshot links work in pool
  and ephemeral mode.
- **jev:** offer filter-apply controls the write gate withheld ([#114](https://github.com/glim-sh/cuttle/pull/114)) ([bdf440a](https://github.com/glim-sh/cuttle/commit/bdf440a86534b7faed6909ceb94f6ed54ca3725f))
  jev-browse no longer withholds a filter panel's apply button ("Apply
  current filters to show results") or the "Easy Apply filter" toggle as
  write-shaped, so a location or job-type filter can be applied instead of
  ending the run with "nothing on this page makes progress".
- **cli:** discarding the profile removes the instance's host snapshot dir ([#113](https://github.com/glim-sh/cuttle/pull/113)) ([57cd07a](https://github.com/glim-sh/cuttle/commit/57cd07ab4c6cae163b37acfbe02dfc84b029e928))
  Discarding the profile (`down --purge`, `purge-profile`, `up
  --purge-profile`) now also removes the instance's host snapshot dir
  (`$XDG_STATE_HOME/cuttle/<name>/snapshots`); a plain `down` leaves it.
  No path changes: `<name>` is the instance name, so the default instance
  keeps `cuttle/cuttle/`.
- **pw:** compact find output, downloads into a directory, quieter logs ([#116](https://github.com/glim-sh/cuttle/pull/116)) ([62f2ca8](https://github.com/glim-sh/cuttle/commit/62f2ca8b36a13c8de858b12f31beb6cc2c28a784))
  `cuttle pw find` prints one line per match with the node's ref and its
  parent's, capped at 40 (`--raw` keeps the driver's output). `cuttle
  downloads <name> <dir>/` and `--latest <dir>/` save under the download's
  name inside it. `cuttle logs` drops tini's PID-1 warning and redacts the
  public IP.

### <!-- 3 -->🚀 Performance
- **cli:** run a pw verb in one docker exec, lease check included ([#100](https://github.com/glim-sh/cuttle/pull/100)) ([a9c3c90](https://github.com/glim-sh/cuttle/commit/a9c3c9030df0ec84d01bc40c8ee394b1ca60aa3c))
  `cuttle pw` verbs now run in a single docker exec, lease check included,
  instead of two or three processes, cutting 30-80 ms off every call.
  Nothing changes in behaviour or output.

### <!-- 4 -->🚜 Refactor
- move the Go module into packages/cuttle ([a728e0b](https://github.com/glim-sh/cuttle/commit/a728e0b97b6d1f9c39e6a0509fb565709af2c424))
- **jev:** the model judges the page, the loop acts on its pick ([#117](https://github.com/glim-sh/cuttle/pull/117)) ([bac0f63](https://github.com/glim-sh/cuttle/commit/bac0f635bdedbdde0274966319e2f36d5e74659f))
  jev-browse now shows the model each page's headings, text and every
  control, and guards writes on the action it picks instead of hiding
  controls: irreversible verbs are refused outright, other picks cost one
  extra model call, and a task needing a write exits 3 naming it. The
  withheld output is gone.

### <!-- 5 -->📚 Documentation
- start the OKF knowledge bundle ([201665b](https://github.com/glim-sh/cuttle/commit/201665b50147e8d892227148eec532a34a06c065))
- **knowledge:** profile dir is the artifact store - relaunch must reuse it ([7d4c430](https://github.com/glim-sh/cuttle/commit/7d4c43014cf5a4fa1ee910b3e45dcbaab2683b80))
- fix README, OPERATING and knowledge drift after jev-browse; complete THIRD-PARTY ([#76](https://github.com/glim-sh/cuttle/pull/76)) ([c1e889e](https://github.com/glim-sh/cuttle/commit/c1e889e1a2299e0f6013f2154a1511468085705d))
  Docs: README no longer says `cuttle open` holds until Ctrl-C or that the
  image is Python-free, jev-browse is marked experimental, and THIRD-PARTY
  now covers the bundled playwright-cli, Node.js and every persona font
  license.
- **knowledge:** capture browse benchmarks, pw call cost, snapshot shape and jev removability ([#104](https://github.com/glim-sh/cuttle/pull/104)) ([0d8152e](https://github.com/glim-sh/cuttle/commit/0d8152e2840a7fecf77e9580e56783796e7a55fa))
  The knowledge bundle now records browse-time benchmarks, the cost of a
  `cuttle pw` call, playwright-cli snapshot behavior, and why
  `internal/jev` stays a removable module. No action needed.
- **skill:** tiny innerText after client-side navigation is an overlay, not a broken page ([#106](https://github.com/glim-sh/cuttle/pull/106)) ([af7f9aa](https://github.com/glim-sh/cuttle/commit/af7f9aae6384b9a9177d436d3f2a3e2b2a29cfd3))
  The embedded skill now tells agents that a few words of innerText right
  after a client-side navigation (URL and title changed) is a transient
  overlay, not a broken page: read `cuttle pw snapshot` instead, or
  `cuttle pw reload` and read again. Guidance only, no behaviour change.
- **knowledge:** merged-main browse benchmark numbers and the stuck-page recovery finding ([#107](https://github.com/glim-sh/cuttle/pull/107)) ([f05f357](https://github.com/glim-sh/cuttle/commit/f05f3575eef40d3b6224ef34d81a9b02228059b4))
  Knowledge base: merged-main browse benchmark numbers and the stuck-page
  recovery finding; nothing changes for users.
- **skill:** goto snapshots at load, so a client-rendered page reads empty at first ([#112](https://github.com/glim-sh/cuttle/pull/112)) ([8f5ec76](https://github.com/glim-sh/cuttle/commit/8f5ec76944d9dcc905220bd08df081de33a34807))
  `cuttle skill` now warns that `goto` returns at `load`, before a
  client-rendered page has drawn: empty containers in its snapshot, or a
  null `eval` right after it, mean the page is still rendering, not a
  broken selector. Wait with `run-code` + `waitForSelector` or `find`,
  then re-snapshot.
- **skill:** subtree snapshot replaces live refs; dialog-accept takes a prompt answer ([#115](https://github.com/glim-sh/cuttle/pull/115)) ([f1dd219](https://github.com/glim-sh/cuttle/commit/f1dd219137a75694ec8a719eb120210722d4262a))
  The embedded skill now says that a subtree `snapshot <ref>` replaces the
  page's live refs (run `find` again before acting outside it) and that a
  `prompt` dialog is answered with `dialog-accept '<text>'`.
- **skill:** point jev-browse at its --help instead of a section ([cfc05d8](https://github.com/glim-sh/cuttle/commit/cfc05d8cb81f4b1dac16ce053567b9f5c12a94e5))

### <!-- 6 -->🧹 Chores
- **smoke:** assert the bundled driver re-attaches to cuttle's browser after it dies ([99c6eaa](https://github.com/glim-sh/cuttle/commit/99c6eaaf61aeccc07e885a9bc270d5d6988fa962))
- add kasetto.yaml ([2083a15](https://github.com/glim-sh/cuttle/commit/2083a15e83e7b157b1eaca2e8d2b7b20e66bf9f4))
- restore the kasetto project config that installs go-dev ([#105](https://github.com/glim-sh/cuttle/pull/105)) ([c32f9bc](https://github.com/glim-sh/cuttle/commit/c32f9bca12f00320ae3174f8081a2c7fd8844815))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.14.2...v0.15.0

## [0.14.2](https://github.com/glim-sh/cuttle/compare/v0.14.1...v0.14.2) - 2026-09-15

### <!-- 2 -->🐛 Bug Fixes
- **cli:** support playwright-cli 0.1.20 ([#63](https://github.com/glim-sh/cuttle/pull/63)) ([def48fd](https://github.com/glim-sh/cuttle/commit/def48fdb219a101362e6d98baf89b67f94adb737))
  The briefing's playwright-cli `docs` command now finds the bundled guide however playwright-cli was installed (mise, pnpm, bun), not only via npm. The guide warns that `recording-start` leaves detectable Playwright globals in the page, and that WebMCP setup does not apply to cuttle's browser.

### <!-- 6 -->🧹 Chores
- **release:** write the release PR as one GitHub-signed commit ([#65](https://github.com/glim-sh/cuttle/pull/65)) ([9703d7b](https://github.com/glim-sh/cuttle/commit/9703d7be1162e70f1f440094b01bf105d056d7b1))

**Full Changelog**: https://github.com/glim-sh/cuttle/compare/v0.14.1...v0.14.2

## [0.14.1](https://github.com/glim-sh/cuttle/compare/v0.14.0...v0.14.1) - 2026-09-07

### <!-- 2 -->🐛 Bug Fixes
- **ci:** drop the blank a filtered footer left, and say when the PAT is missing ([7815dab](https://github.com/glim-sh/cuttle/commit/7815dab7898b89855bf521dc70dd67472a031599))
- **release:** sign and notarize darwin binaries, drop quarantine postflight ([#61](https://github.com/glim-sh/cuttle/pull/61)) ([71784be](https://github.com/glim-sh/cuttle/commit/71784be7487c8104009eea5f657fc68917e67ad5))
  The macOS binaries are now Developer ID signed and notarized. The
  Homebrew cask no longer strips the quarantine attribute in a
  `postflight` hook, so brew stops printing the "Calling `postflight` is
  deprecated" warning on every command.

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
