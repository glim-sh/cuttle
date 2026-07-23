# Spike: Widevine/EME on the linux/arm64 (macOS-persona) build

Status: **deferred research spike / not scheduled.** Out of the critical path for
the main build pipeline (`2607-23-self-hosted-chromium-build-pipeline.md`, R5/Q6).
Do NOT gate arm64 delivery on this. Pick it up only if a real target requires EME
on the arm64 image.

## Why this is a spike and not a task

amd64 Widevine is a tractable Phase 4 enhancement (enable the flag, sideload
Google's `linux-x64` CDM, pre-seed the hint file). **arm64 Linux Widevine is
fundamentally harder** for reasons outside our build: Google never shipped a
desktop `linux-arm64` Widevine CDM, and the one stealth project that tried
(CloakBrowser) never got it working on arm64. So this is exploratory - unknown
effort, real chance it ends in "not worth it."

## The two stacked blockers (both must be solved)

1. **No desktop linux-arm64 CDM exists.** Google publishes `libwidevinecdm.so`
   only for `linux_x64` (inside the Chrome `.deb`). The **only** aarch64 Widevine
   binary Google produces lives inside **ChromeOS LaCrOS** images
   (`_platform_specific/cros_arm64/libwidevinecdm.so`). It **cannot load on vanilla
   Linux as-is** - it needs binary patching:
   - add the `GLIBC_ABI_DT_RELR` version dependency for its `DT_RELR` relocations,
   - inject aarch64 atomic-helper stubs.
   Prior art that does exactly this: [AsahiLinux/widevine-installer](https://github.com/AsahiLinux/widevine-installer)
   (`widevine_fixup.py`, pulls a LaCrOS squashfs from Google's
   `chromeos-localmirror`) and [xesco/pivine](https://github.com/xesco/pivine)
   (`widevine_patch.py`). Corroborated by
   [Mozilla bug 1679354](https://bugzilla.mozilla.org/show_bug.cgi?id=1679354):
   unsupported "due to a lack of a native ARM64 Widevine CDM binary."

2. **The registration code must compile into the arm64 target.** CloakBrowser's
   arm64 build shipped without `widevine_cdm_component_installer.cc`
   ([CloakBrowser #349](https://github.com/CloakHQ/CloakBrowser/issues/349), open,
   no maintainer fix), so its arm64 binary only had stub Widevine strings
   ("Widevine enabled but no library found") and could not register a CDM even if
   supplied. We must confirm our `enable_widevine=true` + `enable_library_cdms=true`
   arm64 build actually compiles the component-installer / host-adapter path -
   `strings` the binary for the `Registering hinted/bundled Widevine` /
   `component_updater` markers the x64 build has.

## The macOS-persona coherence wall (the reason this may be pointless)

Even if both blockers are solved, the arm64 image runs a **macOS persona**. A real
Mac uses a macOS Widevine CDM (`libwidevinecdm.dylib`) - a Mach-O binary that
**will not load on Linux**. So we'd be running a **patched-ChromeOS-arm64 *Linux*
CDM under a macOS persona**. The CDM reports its own platform + security level
(L1/L3) through the EME surface
(`navigator.requestMediaKeySystemAccess(...).then(a => a.getConfiguration())`,
robustness levels), which a real macOS Chrome answers differently. This risks
being a **worse** tell than shipping no CDM at all. Any spike must measure the EME
surface against a real macOS Chrome before concluding it helps.

## Investigation steps (if picked up)

1. **Feasibility gate first (cheapest kill-switch):** on the arm64 target build,
   confirm the registration code compiles in (blocker 2). If it doesn't and wiring
   it is non-trivial, stop - blocker 1's effort is wasted without it.
2. **Obtain + patch the CDM (blocker 1):** run the Asahi/pivine extract+patch flow
   against a current LaCrOS image; get a `libwidevinecdm.so` that `dlopen`s on
   Debian aarch64. Record the LaCrOS version + patch deltas.
3. **Wire it:** sideload the patched CDM + pre-seed the
   `WidevineCdm/latest-component-updated-widevine-cdm` hint file, persistent
   `user-data-dir` (same mechanism as amd64, CloakBrowser #96).
4. **Playback test:** a known EME stream (e.g. Bitmovin DRM demo) plays under Xvfb.
5. **Coherence test (the real go/no-go):** capture the full EME surface
   (supported key systems, robustness/security levels, CDM version string) from our
   arm64/macOS-persona build AND a real macOS Chrome; diff. Ship only if the CDM
   platform/security signal does not contradict the macOS persona. If it does,
   the correct answer is likely **no CDM on arm64** (accept the FingerprintJS
   `nodriver`/high-quota flag, or mitigate storage quota another way).

## Decision criteria

- **Green-light** only if: a specific target gates on EME, AND the coherence test
  (step 5) shows the patched-Linux-CDM doesn't leak non-macOS through the EME
  surface.
- **Otherwise: ship arm64 with no Widevine.** Document the EME gap; if the only
  cost is the FingerprintJS high-quota `nodriver` flag, address that flag directly
  rather than dragging in a ChromeOS CDM.

## References

- [CloakBrowser #349 - arm64 Widevine registration missing](https://github.com/CloakHQ/CloakBrowser/issues/349)
- [CloakBrowser #96 - x64 Widevine working via sideload + hint file](https://github.com/CloakHQ/CloakBrowser/issues/96)
- [AsahiLinux/widevine-installer](https://github.com/AsahiLinux/widevine-installer) (LaCrOS extract + `widevine_fixup.py`)
- [xesco/pivine](https://github.com/xesco/pivine) (`widevine_patch.py`)
- [Mozilla bug 1679354 - no native ARM64 Linux CDM](https://bugzilla.mozilla.org/show_bug.cgi?id=1679354)
- [ungoogled-chromium FAQ - Widevine via sideload](https://ungoogled-software.github.io/ungoogled-chromium-wiki/faq)
