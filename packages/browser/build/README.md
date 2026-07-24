# packages/browser/build - the build driver and its caches

Three files build the stealth-Chromium binary on a Linux host:

- `run-build.sh` - host driver. Builds `Dockerfile.linux` into a build image,
  then runs `build-linux.sh` inside it with `/work` (the persistent Hetzner
  volume) mounted, so the checkout, `out/<cpu>`, and the sccache dir survive
  across container/server teardown.
- `Dockerfile.linux` - the ubuntu:24.04 build image (depot_tools deps + a pinned
  sccache).
- `build-linux.sh` - runs inside the container: sync ungoogled-chromium, apply
  the patch series, `gn gen`, `ninja`, package `stealth-chromium-linux-<cpu>.tar.gz`.

Parametrized by `TARGET_CPU=x64|arm64`. `run-build.sh <foreground|background>`.

## Two caches, and which one actually matters

A warm rebuild is fast because of **two independent** caches. Confusing them
wastes hours:

1. **`out/<cpu>/` - ninja's built objects.** This is the primary incremental
   cache: ninja skips a target whose inputs are unchanged. It is invalidated by
   **mtime/command**, not content - so anything that rewrites a source file's
   mtime (a broad `git checkout -- .`, a `git clean`, a full re-apply of the
   patch series) makes ninja rebuild *everything*, even if the content is
   byte-identical.
2. **`/work/sccache` - sccache's compile cache.** Keyed on **content**
   (preprocessed source + flags). This is the safety net for case 1: if ninja
   recompiles a file whose content is unchanged, sccache returns the cached `.o`
   instantly. Without it, an mtime-invalidated rebuild is a real from-scratch
   compile.

So: keep `out/` warm by touching as little as possible, and keep sccache
**actually caching** so the times you do invalidate `out/` stay cheap.

## Making sccache actually cache (the important part)

`cc_wrapper = "sccache"` in `args.gn` is necessary but **not sufficient**. By
default gn emits two flag families sccache cannot cache, and it silently marks
every such compile "non-cacheable" - the cache stays near-empty and every build
is from scratch. `build-linux.sh` disables both (only when sccache is on):

| gn arg | removes | why it's non-cacheable | safe because |
|---|---|---|---|
| `clang_use_chrome_plugins = false` | `-Xclang -add-plugin` (blink-gc, find-bad-constructs) | sccache can't hash a compiler plugin's behavior | analysis-only checks, no codegen effect; Chromium's own `cc_wrapper.gni` documents disabling it for cache users |
| `use_clang_modules = false` | `-fmodules` + `-Xclang -fmodule*` (libc++ Clang modules) | sccache hard-codes `-fmodules` as `TooHardFlag` -> `CannotCache`, and bails on unknown `-Xclang` args | `docs/modules.md` calls it experimental + not recommended, and slower cold; textual includes emit identical code |
| `use_libcxx_modules = false` | the now-unused libc++ modulemap deps | not a compile flag - this is a per-target dep var, NOT the gate for `-fmodules` (setting it alone was a no-op) | dropping dead deps only |

`use_clang_modules` is the `declare_args()` that actually gates the `-fmodules`
blocks in `build/config/compiler/BUILD.gn`. Chromium already force-disables
modules for `use_reclient` and `cc_wrapper == "icecc"` ("don't handle headers in
modulemap config") but not for sccache, so we set it explicitly.

`chrome_pgo_phase = 0` (PGO off) is already set for the same reason - a PGO
profile makes compiles non-cacheable too.

These are build-hygiene flags, not fingerprint flags: the emitted binary behaves
identically, so the amd64 zero-diff behavioral parity gate is unaffected.

### Verify cache health

```bash
# inside the build container (or wherever SCCACHE_DIR points):
sccache --show-stats
```

- **Healthy:** `Non-cacheable calls` ~0; on a warm rebuild `Cache hits` is the
  large majority of `Compile requests`.
- **Broken:** a big `Non-cacheable calls` with a `Non-cacheable reasons:` list
  (e.g. `-fmodules`, `-Xclang`) - a new default flag slipped in; add the gn arg
  that removes it here.

## Incremental-rebuild gotchas

- **Changing an already-applied patch won't re-apply.** Stage 4 skips any patch
  with a `.browser-applied/<name>.done` marker (see `build-linux.sh`). After
  editing a `.patch`, clear its marker (or `rm -rf build/src/.browser-applied`)
  so it re-applies. The tree also already holds the old version of that patch, so
  re-applying to the dirty tree fuzzes/skips - revert the affected files first.
- **Revert surgically.** To re-apply one changed patch, revert only the files it
  touches (`git checkout -- <those files>`) and clear only its marker. A
  whole-tree `git checkout -- .` reverts *every* patched file's mtime and forces
  ninja to rebuild all ~80k targets - only cheap if sccache is healthy.
- `BROWSER_NO_SCCACHE=1` opts out of sccache (and of the two flags above).
