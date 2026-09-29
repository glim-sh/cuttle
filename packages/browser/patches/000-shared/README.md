# Shared infrastructure for cuttle-stealth-chromium

These are **NEW files** added to the Chromium tree — not diffs against
existing files. The numbered patches consume what's defined here.

## Files

| File | Purpose |
|---|---|
| `cuttle_fingerprint_switches.h/.cc` | All `--fingerprint-*` switch names in one place |
| `cuttle_seed.h/.cc` | Deterministic seed → per-vector default mapping; used by ~15 consumer patches |

`build/build-linux.sh` (stage 5) does the integration: it copies all four
files into `third_party/blink/common/` and adds the two `.cc` files to that
directory's `BUILD.gn` sources, so they are compiled once, into `blink_common`,
which the browser and renderer both link. It also copies the two headers into
`chrome/common/`, because consumer patches include them by either path; the
copies are identical and share one include guard.

## Why a shared header

Without this, 19 patches each register their own CLI switch in their
own ad-hoc spot, fight over header ordering, and produce inconsistent
behavior when a flag is missing. The shared header is the single source
of truth.

## Why deterministic SipHash for defaults

Behavioral contract of `--fingerprint=<seed>`:
- Same seed → same fingerprint across launches
- Different seeds → may produce different fingerprints

SipHash gives us a fast, well-distributed mapping from a string seed to
a 64-bit value. Already in BoringSSL (in-tree at
`third_party/boringssl/src/include/openssl/siphash.h`) — we're not adding
a dep.

## Why a per-vector key, not just the seed

Without a vector key, every vector would derive its value from the same
SipHash output (modded down). That means a single seed → predictable
correlations between vectors — exactly the cluster signal detectors look
for. Hashing `(seed, "hwc")` and `(seed, "devmem")` independently
decouples them.

## Why the values are NOT secrets

`kKey` is fixed and visible. The point isn't unguessable; it's
**reproducible**. A determined detection service could derive our
default-tables for a given seed — but those defaults are plausible Chrome
profiles. There's nothing to hide.

## Integration acceptance test

After integrating, `out/Default/chrome --fingerprint=42069 --headless=new
--remote-debugging-port=9333` should:

1. Start and respond to `/json/version`
2. Crash-free for 30 seconds
3. The console.log of `cuttle::seed::Hash("hwc")` (added temporarily) is
   stable across runs

Once those pass, swap to consumer patches.
