// Copyright 2026 Clark Labs Inc. SPDX-License-Identifier: BSD-3-Clause

#include "chrome/common/cuttle_seed.h"

#include <array>
#include <cstring>
#include <string>

#include "base/command_line.h"
#include "base/rand_util.h"
#include "base/strings/string_number_conversions.h"
#include "base/strings/string_util.h"
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

std::string SeedString() {
  auto* cl = base::CommandLine::ForCurrentProcess();
  if (cl->HasSwitch(cuttle::switches::kFingerprint))
    return cl->GetSwitchValueASCII(cuttle::switches::kFingerprint);
  return std::string();  // empty seed = "auto" → still deterministic for
                         // the current process via PID-derived fallback
                         // in Hash() below.
}

struct NetworkProfileDefinition {
  const char* name;
  const char* connection_type;
  const char* effective_type;
  uint32_t min_rtt_msec;
  uint32_t rtt_span_msec;
  uint32_t min_downlink_tenths;
  uint32_t downlink_span_tenths;
};

const NetworkProfileDefinition& DesktopNetworkProfile() {
  static constexpr NetworkProfileDefinition kProfile = {
      "desktop", "wifi", "4g", 35, 90, 80, 260};
  return kProfile;
}

const NetworkProfileDefinition& NetworkProfileForName(std::string name) {
  static constexpr NetworkProfileDefinition kProfiles[] = {
      {"residential", "wifi", "4g", 45, 130, 60, 240},
      {"datacenter", "ethernet", "4g", 10, 55, 300, 900},
      {"mobile", "cellular", "4g", 70, 170, 20, 130},
      {"slow", "cellular", "3g", 250, 450, 4, 24},
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
  std::string seed = SeedString();
  if (seed.empty()) {
    // Fallback: per-process random — picks a stable identity for this
    // process but different across launches. Matches CloakBrowser's
    // README claim of "stealthy by default; auto-generated random seed".
    static const uint64_t k_proc = base::RandUint64();
    std::string combined;
    combined.reserve(8 + key.size());
    combined.append(reinterpret_cast<const char*>(&k_proc), 8);
    combined.append(key);
    return SIPHASH_24(kKey,
                      reinterpret_cast<const uint8_t*>(combined.data()),
                      combined.size());
  }
  std::string combined = seed;
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
        v > 0 && v <= 1024) {
      return v;
    }
  }
  static constexpr uint32_t kChoices[] = {4, 6, 8, 12, 16};
  return Pick(kChoices, "hwc");
}

double DeviceMemoryGB() {
  auto* cl = base::CommandLine::ForCurrentProcess();
  if (cl->HasSwitch(cuttle::switches::kFingerprintDeviceMemory)) {
    double v = 0;
    if (base::StringToDouble(
            cl->GetSwitchValueASCII(
                cuttle::switches::kFingerprintDeviceMemory), &v) &&
        v > 0 && v <= 64) {
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
  // as the k_proc fallback in Hash() below.
  static const ScreenSize kValue = []() -> ScreenSize {
    auto* cl = base::CommandLine::ForCurrentProcess();
    uint32_t w = 0, h = 0;
    base::StringToUint(
        cl->GetSwitchValueASCII(cuttle::switches::kFingerprintScreenWidth), &w);
    base::StringToUint(
        cl->GetSwitchValueASCII(cuttle::switches::kFingerprintScreenHeight), &h);
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
  auto* cl = base::CommandLine::ForCurrentProcess();
  if (cl->HasSwitch(cuttle::switches::kFingerprintTaskbarHeight)) {
    unsigned v = 0;
    if (base::StringToUint(
            cl->GetSwitchValueASCII(
                cuttle::switches::kFingerprintTaskbarHeight), &v) &&
        v < 200) {
      return v;
    }
  }
  std::string plat = cl->GetSwitchValueASCII(
      cuttle::switches::kFingerprintPlatform);
  if (plat == "macos") return 95;
  if (plat == "linux") return 0;
  return 48;  // windows default
}

NetworkQuality Network() {
  auto* cl = base::CommandLine::ForCurrentProcess();
  const auto& profile = NetworkProfileForName(
      cl->GetSwitchValueASCII(cuttle::switches::kFingerprintNetworkProfile));

  NetworkQuality value = {
      profile.connection_type,
      profile.effective_type,
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

  std::string effective_type = base::ToLowerASCII(
      cl->GetSwitchValueASCII(cuttle::switches::kFingerprintEffectiveType));
  if (const char* canonical = CanonicalEffectiveType(effective_type))
    value.effective_type = canonical;

  unsigned rtt = 0;
  if (base::StringToUint(
          cl->GetSwitchValueASCII(cuttle::switches::kFingerprintRtt), &rtt) &&
      rtt > 0 && rtt <= 5000) {
    value.rtt_msec = rtt;
  }

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

}  // namespace cuttle::seed
