# Spike: Widevine/EME on the linux/arm64 (macOS-persona) build

Status: **blocker 1 resolved 2026-08-19 — a first-party linux-arm64 CDM now
exists.** The original spike (2026-07-23) concluded arm64 Widevine was
impractical. That conclusion was correct when written and is now wrong: Google
shipped an official Linux arm64 Widevine CDM in **M149 (2026-06-02)**, six weeks
after this doc was authored.

What remains open is not availability but **persona coherence** (see below).

## What changed

The commit is [`widevine: Bundle Linux arm64 CDM`](https://github.com/chromium/chromium/commit/e94ddd228c22bc36c308326f43679ccf89826592)
(2026-04-30, commit position 1622854), which added an arm64 slice to Google's
internal CDM drop. Position 1622854 sits between the M148 branch (1610480) and
the M149 branch (1625079), so it first shipped in **M149 stable**.

Product timeline, which explains why every reference implementation still
assumes otherwise: Google [announced Chrome for ARM64 Linux](https://blog.google/chromium/bringing-chrome-to-arm64-linux-devices)
on 2026-03-12, the arm64 `.deb`/`.rpm` appeared unannounced around 2026-07-26,
and support pages only listed Linux ARM on **2026-08-16**. clearcote's fetcher
(`sdk/python/clearcote/_widevine.py`) is still x64-only for this reason.

## Blocker 1 (no arm64 CDM) — RESOLVED

The CDM comes from the **same Omaha endpoint the x64 path already uses**
(`https://update.googleapis.com/service/update2/json`), with `arch: "arm64"` and
`prodversion >= 149`. No ChromeOS extraction, no binary patching.

The official blob is materially cleaner than the old ChromeOS-derived route:

| property | ChromeOS-derived 4.10.2662.3 | **official 4.10.3057.0** |
|---|---|---|
| `DT_RELR` | present (needs `GLIBC_ABI_DT_RELR` injection) | **absent** |
| libc version dep | `GLIBC_2.17`, `2.28`, `ABI_DT_RELR` | **`GLIBC_2.17` only** |
| `libm` | `GLIBC_2.29` | `GLIBC_2.17` |
| injected atomic stubs | `__aarch64_{ldadd4,swp4}_acq_rel` | **none** |

Verified working on native arm64 Docker: `Registering hinted Widevine
4.10.3057.0` plus a successful `createMediaKeys()`. Loads on 4K/16K/64K pages.

The old ecosystem this doc pointed at is also decaying and should not be used:
Raspberry Pi OS's `libwidevinecdm0` has not updated since 2023-10-25, the AUR
`widevine-aarch64` package was deleted, and Kodi's LaCrOS route was removed in
2025 after its URLs died. (Raspberry Pi's arm64 package shares BuildID
`109fbe02…` with the Asahi LaCrOS blob, confirming it was the patched ChromeOS
CDM rather than a Pi-specific agreement.)

## Blocker 2 (registration compiles into the arm64 target) — resolved by config

In Chromium 151 `third_party/widevine/cdm/widevine.gni`:

- `enable_widevine` defaults false for unbranded builds; upstream explicitly
  sanctions enabling it on non-Android platforms.
- `library_widevine_cdm_available` is **true for `target_os == "linux"` with
  `target_cpu` of either `x64` or `arm64`**.
- `enable_widevine_cdm_component` is true on Linux — the CDM is delivered by
  component update at runtime.
- `bundle_widevine_cdm` stays **false** unless chrome-branded.

`packages/browser/build/build-linux.sh` now sets `enable_widevine = true` for
both targets. That last row is the licensing-critical one: we compile the key
system support but ship **no proprietary blob** in our binary, tarballs or image.
The operator's container fetches the CDM at runtime. ungoogled's own `flags.gn`
sets `enable_widevine=true` for the same reason.

CloakBrowser #349 (arm64 registration missing) described their build, not a
Chromium limitation, and does not apply to ours.

## The macOS-persona coherence wall — STILL OPEN

This is now the only real question, and the original doc was right to raise it.

The arm64 image presents a **macOS persona**, but a real Mac loads
`libwidevinecdm.dylib` (Mach-O). We would be running a **Linux arm64 CDM under a
macOS persona**. The CDM reports its own platform and security level through the
EME surface (`navigator.requestMediaKeySystemAccess(...)` →
`getConfiguration()`, robustness levels), which a real macOS Chrome answers
differently.

Against that: shipping **no** CDM is itself a contradiction, because a build
claiming to be Chrome carries Widevine. clearcote measured the x64/Windows case
and found seeding it moved their audit 97/100 → 98/100 and identity 58/61 →
59/61. So both states are imperfect and the question is which is less detectable.

**This must be measured, not reasoned about.** Capture the full EME surface from
our arm64 build and from a real macOS Chrome, and diff.

## Practical constraints (apply to both arches)

- **Startup-only registration.** The CDM is registered before the zygote locks
  down, so it cannot be hot-swapped or lazily fetched into a running browser. The
  fetch must complete **before `cuttle serve` launches Chrome for a seed.**
- **L3 ceiling.** Linux is Widevine L3 regardless of arch;
  [Netflix caps Linux + Chrome at 720p](https://help.netflix.com/en/node/30081)
  versus 2160p on Windows/Chromebook. Irrelevant if the goal is only that EME be
  *present*; a hard ceiling if playback quality ever matters. L1 lives in a
  separate ChromeOS `cdm-oemcrypto` daemon with factory-provisioned keys and is
  not obtainable from any `.so`.
- **Emulation is not a fallback.** Chromium did run an x86_64 CDM under Rosetta
  on macOS in M88, so the concept is proven — but it was removed once a native
  arm64 CDM landed, never existed for Linux, and Linux is structurally worse
  because the CDM is preloaded in the zygote via `fork()` rather than `exec()`,
  leaving nowhere to insert an emulator without `--no-zygote`.

## Remaining steps

1. Build the arch-generic fetcher (`linux-amd64` + `linux-arm64`) against the
   Omaha endpoint, gated per persona with a `CUTTLE_WIDEVINE=1/0` opt-out, run
   before Chrome launch.
2. Verify registration in the shipped binaries: `Registering hinted Widevine` in
   the log, and `createMediaKeys()` resolving.
3. **Coherence test (the go/no-go for arm64):** diff the EME surface of our
   arm64/macOS-persona build against a real macOS Chrome. Ship arm64 Widevine
   only if the CDM platform/security signal does not contradict the persona.
4. Empirical check, low priority: the ChromeOS-blob route historically needed a
   `CrOS aarch64` UA spoof for some services. That was service-side gating on the
   UA string, not a property of the binary, and no such report exists for
   official Chrome arm64 — but it would conflict hard with the macOS persona, so
   confirm if DRM playback ever becomes a target.

## References

- [chromium `widevine: Bundle Linux arm64 CDM`](https://github.com/chromium/chromium/commit/e94ddd228c22bc36c308326f43679ccf89826592) (M149, the change that obsoleted this spike)
- [Chrome for ARM64 Linux announcement](https://blog.google/chromium/bringing-chrome-to-arm64-linux-devices)
- [Netflix playback-quality limits by platform](https://help.netflix.com/en/node/30081) (the L3 720p ceiling)
- [CloakBrowser #96 - x64 Widevine via sideload + hint file](https://github.com/CloakHQ/CloakBrowser/issues/96)
- [CloakBrowser #349 - arm64 registration missing in *their* build](https://github.com/CloakHQ/CloakBrowser/issues/349)
- [ungoogled-chromium FAQ - Widevine via sideload](https://ungoogled-software.github.io/ungoogled-chromium-wiki/faq)

### Superseded by this revision

The 2026-07-23 version concluded arm64 Widevine required extracting and binary-
patching a ChromeOS LaCrOS CDM ([AsahiLinux/widevine-installer](https://github.com/AsahiLinux/widevine-installer),
[xesco/pivine](https://github.com/xesco/pivine)) and cited
[Mozilla bug 1679354](https://bugzilla.mozilla.org/show_bug.cgi?id=1679354) for
"no native ARM64 Widevine CDM binary". All of that was accurate on 2026-07-23 and
is obsolete as of M149. Kept here so the reversal is visible rather than silently
rewritten.
