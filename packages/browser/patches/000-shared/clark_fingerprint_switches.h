// Copyright 2026 The Clark Authors. SPDX-License-Identifier: BSD-3-Clause
//
// clark-stealth-chromium — shared command-line switch names.
//
// All --fingerprint-* switches live here so individual patches don't fight
// over header ownership. Pattern lifted from Chromium's own
// chrome/common/chrome_switches.h.
//
// To add a new switch:
//   1. Add the constexpr extern declaration here.
//   2. Add the definition in clark_fingerprint_switches.cc.
//   3. Reference via clark::switches::kFingerprintFoo in the patch that
//      consumes it.
//
// This is a NEW file added at chrome/common/clark_fingerprint_switches.h
// (added in patch 000-shared, see BUILD.gn fragment in this directory).

#ifndef CHROME_COMMON_CLARK_FINGERPRINT_SWITCHES_H_
#define CHROME_COMMON_CLARK_FINGERPRINT_SWITCHES_H_

namespace clark::switches {

// Master fingerprint seed. Drives all defaults for unset switches below.
// Format: ASCII digits, 1..128 chars. Example: --fingerprint=42069
extern const char kFingerprint[];

// Platform identity. One of: "windows" | "macos" | "linux".
// Affects: navigator.platform, navigator.userAgentData.platform, font
// resolution, WebGL GPU pool selection.
extern const char kFingerprintPlatform[];

// Platform OS version reported via Sec-CH-UA-Platform-Version client hint.
extern const char kFingerprintPlatformVersion[];

// Brand string for Sec-CH-UA: "Chrome" | "Edge" | "Opera" | "Vivaldi".
extern const char kFingerprintBrand[];

// Brand version for Sec-CH-UA. Falls back to current Chromium version.
extern const char kFingerprintBrandVersion[];

// WebGL UNMASKED_VENDOR_WEBGL override (raw string).
extern const char kFingerprintGpuVendor[];

// WebGL UNMASKED_RENDERER_WEBGL override (raw string).
extern const char kFingerprintGpuRenderer[];

// navigator.hardwareConcurrency. Positive int.
extern const char kFingerprintHardwareConcurrency[];

// navigator.deviceMemory in GB. Member of {0.25, 0.5, 1, 2, 4, 8}.
extern const char kFingerprintDeviceMemory[];

// screen.width. Positive int.
extern const char kFingerprintScreenWidth[];

// screen.height. Positive int.
extern const char kFingerprintScreenHeight[];

// Subtracted from screen height to derive availHeight. Win=48, Mac=95,
// Linux=0 unless overridden.
extern const char kFingerprintTaskbarHeight[];

// navigator.storage.estimate().quota in MB.
extern const char kFingerprintStorageQuota[];

// IANA timezone (e.g. "America/New_York"). Sets ICU default zone in
// every renderer process.
extern const char kFingerprintTimezone[];

// BCP-47 locale (e.g. "en-US"). Also drives --lang.
extern const char kFingerprintLocale[];

// Directory containing target-platform fonts. The Python launcher exposes this
// through Fontconfig on Linux; native FontCache plumbing is tracked separately.
extern const char kFingerprintFontsDir[];

// Geolocation lat,lon (e.g. "40.7128,-74.0060").
extern const char kFingerprintLocation[];

// WebRTC ICE candidate IP replacement. Literal IPv4 string.
extern const char kFingerprintWebrtcIp[];

// navigator.maxTouchPoints. Integer in [0, 16]. Desktop default is 0.
extern const char kFingerprintMaxTouchPoints[];

// AudioContext.sampleRate. Integer in {44100, 48000}.
extern const char kFingerprintAudioSampleRate[];

// navigator.connection network profile. One of:
// "desktop" | "residential" | "datacenter" | "mobile" | "slow".
extern const char kFingerprintNetworkProfile[];

// navigator.connection.type override. One of:
// "wifi" | "ethernet" | "cellular" | "bluetooth" | "wimax" |
// "other" | "none" | "unknown".
extern const char kFingerprintConnectionType[];

// navigator.connection.effectiveType override.
// One of: "slow-2g" | "2g" | "3g" | "4g".
extern const char kFingerprintEffectiveType[];

// navigator.connection.rtt override in milliseconds.
extern const char kFingerprintRtt[];

// navigator.connection.downlink override in megabits per second.
extern const char kFingerprintDownlink[];

// Disable canvas/WebGL/audio noise overlay while keeping the
// deterministic seed in effect. Value: "true" | "false". Default: not set
// (= noise on).
extern const char kFingerprintNoise[];

}  // namespace clark::switches

#endif  // CHROME_COMMON_CLARK_FINGERPRINT_SWITCHES_H_
