# Shared infrastructure for clark-stealth-chromium

These are **NEW files** added to the Chromium tree — not diffs against
existing files. The other 49 patches consume what's defined here.

## Files

| File | Goes to | Purpose |
|---|---|---|
| `clark_fingerprint_switches.h/.cc` | `chrome/common/` | All `--fingerprint-*` switch names in one place |
| `clark_seed.h/.cc` | `chrome/common/` | Deterministic seed → per-vector default mapping; used by ~15 consumer patches |
| `BUILD.gn.fragment` | `chrome/common/BUILD.gn` | How to wire the above into the build |

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
3. The console.log of `clark::seed::Hash("hwc")` (added temporarily) is
   stable across runs

Once those pass, swap to consumer patches.
