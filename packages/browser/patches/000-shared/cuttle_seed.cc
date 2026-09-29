// Copyright 2026 Clark Labs Inc. SPDX-License-Identifier: BSD-3-Clause

#include "chrome/common/cuttle_seed.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <string>

#include "base/command_line.h"
#include "base/no_destructor.h"
#include "base/rand_util.h"
#include "base/strings/string_number_conversions.h"
#include "base/strings/string_util.h"
#include "base/version.h"
#include "chrome/common/cuttle_fingerprint_switches.h"

// SipHash from BoringSSL. Public API. Already in-tree under
// third_party/boringssl/; we're not adding a dep.
#include "third_party/boringssl/src/include/openssl/siphash.h"

namespace cuttle::seed {

namespace {

// Fixed 16-byte key as two uint64_t — BoringSSL SIPHASH_24 signature.
// NOT a secret — purpose is deterministic mapping, not security.
constexpr uint64_t kKey[2] = {
    0x0123456789ABCDEFULL,  // not a secret — just a stable mapping seed
    0xFEDCBA9876543210ULL,
};

// Memoized: Hash() runs on hot paths (canvas, audio, WebGL noise), and the
// command line is immutable after process start. An empty seed means "auto":
// the browser process draws one, and patch #50 passes that value to every
// renderer, so a page's out-of-process frames and workers - and the browser's
// own consumers (patch #65) - share one identity instead of one per process.
const std::string& SeedString() {
  static const base::NoDestructor<std::string> kSeed([] {
    std::string seed =
        base::CommandLine::ForCurrentProcess()->GetSwitchValueASCII(
            cuttle::switches::kFingerprint);
    return seed.empty() ? base::NumberToString(base::RandUint64()) : seed;
  }());
  return *kSeed;
}

struct NetworkProfileDefinition {
  const char* name;
  const char* connection_type;
  uint32_t min_rtt_msec;
  uint32_t rtt_span_msec;
  uint32_t min_downlink_tenths;
  uint32_t downlink_span_tenths;
};

const NetworkProfileDefinition& DesktopNetworkProfile() {
  static constexpr NetworkProfileDefinition kProfile = {
      "desktop", "wifi", 35, 90, 80, 260};
  return kProfile;
}

const NetworkProfileDefinition& NetworkProfileForName(std::string name) {
  static constexpr NetworkProfileDefinition kProfiles[] = {
      {"residential", "wifi", 45, 130, 60, 240},
      // Floor 30: Blink scales rtt by up to 0.9 and rounds to 50ms, so a lower
      // one reports rtt=0, the headless tell this profile exists to avoid.
      {"datacenter", "ethernet", 30, 35, 300, 900},
      {"mobile", "cellular", 70, 170, 20, 130},
      {"slow", "cellular", 300, 400, 4, 24},
  };

  name = base::ToLowerASCII(name);
  for (const auto& profile : kProfiles) {
    if (name == profile.name)
      return profile;
  }
  return DesktopNetworkProfile();
}

const char* CanonicalConnectionType(std::string_view value) {
  if (value == "wifi") return "wifi";
  if (value == "ethernet") return "ethernet";
  if (value == "cellular") return "cellular";
  if (value == "bluetooth") return "bluetooth";
  if (value == "wimax") return "wimax";
  if (value == "other") return "other";
  if (value == "none") return "none";
  if (value == "unknown") return "unknown";
  return nullptr;
}

// The network quality estimator's http-rtt thresholds
// (kHttpRttEffectiveConnectionTypeThresholds in
// net/nqe/network_quality_estimator_params.h); no throughput threshold is set.
const char* EffectiveTypeForRtt(uint32_t rtt_msec) {
  if (rtt_msec >= 2010) return "slow-2g";
  if (rtt_msec >= 1420) return "2g";
  if (rtt_msec >= 272) return "3g";
  return "4g";
}

const char* CanonicalEffectiveType(std::string_view value) {
  if (value == "slow-2g") return "slow-2g";
  if (value == "2g") return "2g";
  if (value == "3g") return "3g";
  if (value == "4g") return "4g";
  return nullptr;
}

}  // namespace

std::string Get() { return SeedString(); }

uint64_t Hash(std::string_view key) {
  std::string combined = SeedString();
  combined.push_back('|');
  combined.append(key);
  return SIPHASH_24(kKey,
                    reinterpret_cast<const uint8_t*>(combined.data()),
                    combined.size());
}

uint32_t HardwareConcurrency() {
  auto* cl = base::CommandLine::ForCurrentProcess();
  if (cl->HasSwitch(cuttle::switches::kFingerprintHardwareConcurrency)) {
    unsigned v = 0;
    if (base::StringToUint(
            cl->GetSwitchValueASCII(
                cuttle::switches::kFingerprintHardwareConcurrency), &v) &&
        v > 0 && v <= 256) {  // the same range patch #06 accepts
      return v;
    }
  }
  static constexpr uint32_t kChoices[] = {4, 6, 8, 12, 16};
  return Pick(kChoices, "hwc");
}

double DeviceMemoryGB() {
  auto* cl = base::CommandLine::ForCurrentProcess();
  if (cl->HasSwitch(cuttle::switches::kFingerprintDeviceMemory)) {
    // Only a value desktop Chrome can report: ApproximatedDeviceMemory rounds
    // to a power of two and clamps to 2..32 GiB.
    unsigned v = 0;
    if (base::StringToUint(
            cl->GetSwitchValueASCII(
                cuttle::switches::kFingerprintDeviceMemory), &v) &&
        v >= 2 && v <= 32 && (v & (v - 1)) == 0) {
      return v;
    }
  }
  static constexpr double kChoices[] = {4.0, 8.0};
  return Pick(kChoices, "devmem");
}

ScreenSize Screen() {
  // Memoized. The command line is immutable after process start, and patch #54
  // put this on CSS media evaluation - so an un-cached version would re-parse
  // the switch map, allocate two std::strings and (on a seed-default launch)
  // run a SipHash on EVERY (device-width) / (device-height) query. Same idiom
  // as SeedString() above.
  static const ScreenSize kValue = []() -> ScreenSize {
    auto* cl = base::CommandLine::ForCurrentProcess();
    // Consumers do signed int math, and availHeight subtracts a taskbar of up
    // to 199px, so a value must parse whole and sit in a real display's range.
    const auto parse = [cl](const char* name) -> uint32_t {
      unsigned v = 0;
      return base::StringToUint(cl->GetSwitchValueASCII(name), &v) &&
                     v >= 480 && v <= 16384
                 ? v
                 : 0;
    };
    const uint32_t w = parse(cuttle::switches::kFingerprintScreenWidth);
    const uint32_t h = parse(cuttle::switches::kFingerprintScreenHeight);
    if (w > 0 && h > 0) return ScreenSize{w, h};

    // Coherent pairs only - never split width/height across pairs.
    static constexpr ScreenSize kChoices[] = {
        {1920, 1080}, {1536, 864}, {2560, 1440}, {1366, 768}, {1440, 900},
    };
    return Pick(kChoices, "screen");
  }();
  return kValue;
}

double DevicePixelRatio() {
  // Memoized for the same reason as Screen(): patch #54 reads this from
  // Document::DevicePixelRatio(), i.e. once per srcset / image-set() selection
  // and per DPR client hint, where returning a std::string by value each time
  // is pure waste.
  static const double kValue = []() -> double {
    const std::string plat = base::CommandLine::ForCurrentProcess()
                                 ->GetSwitchValueASCII(
                                     cuttle::switches::kFingerprintPlatform);
    if (plat.empty()) return 0.0;  // no persona - caller keeps the real ratio
    return plat == "macos" ? 2.0 : 1.0;
  }();
  return kValue;
}

uint32_t TaskbarHeight() {
  // Memoized: screen.availHeight and the window geometry read it.
  static const uint32_t kValue = []() -> uint32_t {
    auto* cl = base::CommandLine::ForCurrentProcess();
    unsigned v = 0;
    if (base::StringToUint(
            cl->GetSwitchValueASCII(
                cuttle::switches::kFingerprintTaskbarHeight), &v) &&
        v < 200) {
      return v;
    }
    const std::string plat = cl->GetSwitchValueASCII(
        cuttle::switches::kFingerprintPlatform);
    if (plat == "macos") return 95;
    if (plat == "linux") return 0;
    return 48;  // windows default
  }();
  return kValue;
}

uint32_t MenuBarHeight() {
  // Memoized: window.screenY and every mouse and pointer event read it.
  static const uint32_t kValue = []() -> uint32_t {
    if (base::CommandLine::ForCurrentProcess()->GetSwitchValueASCII(
            cuttle::switches::kFingerprintPlatform) != "macos") {
      return 0;
    }
    // NSScreen.visibleFrame's top inset, which is what Chrome on a Mac reports:
    // the bar plus one point. macOS 26 draws a 29pt bar on a notchless screen
    // (30 measured on real Chrome 154); a notched panel wraps the camera in a
    // 37pt strip at its default scaling. 1440x900 is the only notchless Mac in
    // the persona screen table (MacBook Air M1).
    const uint32_t bar = Screen().height == 900 ? 30 : 38;
    return std::min(bar, TaskbarHeight());
  }();
  return kValue;
}

NetworkQuality Network() {
  // Raw estimator values: patch #51 hands rtt and downlink to Blink's
  // RoundRtt()/RoundMbps(), so the page sees what stock Chrome reports (rtt a
  // multiple of 50 up to 3000, downlink a multiple of 0.05 up to 10, both
  // scaled per host).
  auto* cl = base::CommandLine::ForCurrentProcess();
  const auto& profile = NetworkProfileForName(
      cl->GetSwitchValueASCII(cuttle::switches::kFingerprintNetworkProfile));

  NetworkQuality value = {
      profile.connection_type,
      nullptr,
      profile.min_rtt_msec +
          static_cast<uint32_t>(Hash("net.rtt") %
                                (profile.rtt_span_msec + 1)),
      static_cast<double>(
          profile.min_downlink_tenths +
          static_cast<uint32_t>(Hash("net.downlink") %
                                (profile.downlink_span_tenths + 1))) /
          10.0,
  };

  std::string connection_type = base::ToLowerASCII(
      cl->GetSwitchValueASCII(cuttle::switches::kFingerprintConnectionType));
  if (const char* canonical = CanonicalConnectionType(connection_type))
    value.connection_type = canonical;

  unsigned rtt = 0;
  if (base::StringToUint(
          cl->GetSwitchValueASCII(cuttle::switches::kFingerprintRtt), &rtt) &&
      rtt > 0 && rtt <= 5000) {
    value.rtt_msec = rtt;
  }

  // Chrome derives effectiveType from the same rtt estimate, so only an
  // explicit --fingerprint-effective-type can make the two disagree.
  value.effective_type = EffectiveTypeForRtt(value.rtt_msec);
  std::string effective_type = base::ToLowerASCII(
      cl->GetSwitchValueASCII(cuttle::switches::kFingerprintEffectiveType));
  if (const char* canonical = CanonicalEffectiveType(effective_type))
    value.effective_type = canonical;

  double downlink = 0;
  if (base::StringToDouble(
          cl->GetSwitchValueASCII(cuttle::switches::kFingerprintDownlink),
          &downlink) &&
      downlink > 0 && downlink <= 10000) {
    value.downlink_mbps = downlink;
  }

  return value;
}

bool NoiseEnabled() {
  auto* cl = base::CommandLine::ForCurrentProcess();
  if (cl->HasSwitch(cuttle::switches::kFingerprintNoise)) {
    std::string v = cl->GetSwitchValueASCII(
        cuttle::switches::kFingerprintNoise);
    if (base::ToLowerASCII(v) == "false" || v == "0") return false;
  }
  return true;
}

std::string BrandVersion() {
  std::string value = base::CommandLine::ForCurrentProcess()->GetSwitchValueASCII(
      cuttle::switches::kFingerprintBrandVersion);
  const base::Version version(value);
  if (!version.IsValid()) return std::string();
  const auto& parts = version.components();
  if ((parts.size() != 1 && parts.size() != 4) || parts[0] < 1 ||
      parts[0] > 999) {
    return std::string();
  }
  return value;
}

}  // namespace cuttle::seed
