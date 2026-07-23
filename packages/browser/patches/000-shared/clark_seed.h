// Copyright 2026 Clark Labs Inc. SPDX-License-Identifier: BSD-3-Clause
//
// clark-stealth-chromium — seed-derived defaults.
//
// Many fingerprint vectors take a default value when their specific
// --fingerprint-* switch is absent. These defaults must be:
//   1. Deterministic — same --fingerprint=<seed> → same values across runs
//   2. Coherent — defaults for related vectors must form a plausible
//      profile (a screen of 1920x1080 should pair with availHeight ~1032
//      on Windows, not 880).
//   3. Cross-platform safe — defaults for --fingerprint-platform=windows
//      pick a Windows GPU pool, not an Apple one.
//
// This file owns the deterministic mapping seed → per-vector default.
// All consumers MUST go through these helpers; no one rolls their own
// hash-of-seed logic in a patch.
//
// Design: use SipHash (already available in BoringSSL under
// `third_party/boringssl/src/include/openssl/siphash.h`) to convert seed
// string + per-vector key string into a uint64_t, then map to the
// vector's value space.

#ifndef CHROME_COMMON_CLARK_SEED_H_
#define CHROME_COMMON_CLARK_SEED_H_

#include <cstdint>
#include <string>
#include <string_view>

namespace clark::seed {

// Returns the seed string set via --fingerprint, or "" if unset.
std::string Get();

// Deterministic 64-bit hash of (seed, key). `key` is a per-vector
// constant chosen by the consumer (e.g. "hwc", "devmem", "screen.w").
uint64_t Hash(std::string_view key);

// Convenience: pick one element from a constant array deterministically.
template <typename T, size_t N>
const T& Pick(const T (&choices)[N], std::string_view key) {
  return choices[Hash(key) % N];
}

// --- Per-vector defaults (used when the specific switch is unset). ---

// navigator.hardwareConcurrency — one of {4, 6, 8, 12, 16}.
uint32_t HardwareConcurrency();

// navigator.deviceMemory — one of {4.0, 8.0}.
double DeviceMemoryGB();

// screen.width × screen.height — one of a small list of common pairs:
//   1920x1080, 1536x864, 2560x1440, 1366x768, 1440x900
struct ScreenSize { uint32_t width; uint32_t height; };
ScreenSize Screen();

// Taskbar height — Win=48, Mac=95, Linux=0, picked from
// --fingerprint-platform (or "windows" default).
uint32_t TaskbarHeight();

// navigator.connection defaults. Values are deterministic for the same
// --fingerprint seed and are shaped by --fingerprint-network-profile when set.
struct NetworkQuality {
  const char* connection_type;
  const char* effective_type;
  uint32_t rtt_msec;
  double downlink_mbps;
};
NetworkQuality Network();

// Whether canvas/WebGL/audio noise is enabled (default true). False if
// --fingerprint-noise=false explicitly.
bool NoiseEnabled();

}  // namespace clark::seed

#endif  // CHROME_COMMON_CLARK_SEED_H_
