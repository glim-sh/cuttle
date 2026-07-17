# Native macOS backend (local, VNC-less, macOS persona)

Status: implemented. Design note for the native darwin backend added in this
change; kept as the rationale record for the persona choice and window handoff.

## Goal

On Apple Silicon macOS, drop the Rosetta-emulated `linux/amd64` Docker path for
local, interactive use. Run clark's native `darwin-arm64` stealth Chromium
directly under `cuttle serve` (no Docker, no Xvfb, no KasmVNC), and hand off to
the user by surfacing the real native browser window instead of a VNC viewer.

This backend is **local-only and macOS-only**. It is NOT meant to match the
server/Docker fingerprint. Production Windows-persona scraping stays on the
Docker/Linux path unchanged.

## Why macOS persona (measured, not assumed)

Empirical check of clark `chromium-148.0.7778.96-stealth5` `darwin-arm64`,
native, headed, on M1 Max (probe in scratchpad, 2026-07-17):

- `--fingerprint-platform=windows`: UA-CH `architecture: x86` (NOT arm - clark
  spoofs it), no renderer crash on browserleaks webrtc/webgl/canvas. So
  CloakBrowser #444 (arm UA-CH leak) and #396 (windows-spoof crash) do NOT
  reproduce on clark.
- BUT WebGL unmasked renderer = `ANGLE (Apple, ANGLE Metal Renderer: Apple M1
  Max)` under BOTH personas. The real GPU is unspoofable on the mac build.
  Incoherent with Windows (cuttle's `SKILL.md` requires a spoofed D3D11 pair);
  coherent with macOS (a real M1 Mac reports exactly this + frozen
  `Intel Mac OS X 10_15_7` Chrome UA).

Conclusion: native arm64 forces the macOS persona - and that persona is the
strongest possible fingerprint (a genuine Mac on a Mac), so it is the right
choice, not a compromise.

## Design

### Persona (fingerprint)
- `getDefaultStealthArgs` already returns `--fingerprint-platform=macos` on
  darwin. Good, no change.
- `ForkParityArgs` currently hardcodes the Windows persona (UA, `/opt/winfonts`,
  platform=windows, brand). Make it branch on `systemName()`:
  - darwin: minimal set - the noise switches
    (`--fingerprinting-client-rects-noise`, canvas measuretext/image-data
    noise), `--accept-lang`, and `--fingerprint-network-profile=residential`
    (proxy). NO `--user-agent`, NO `--fingerprint-fonts-dir`, NO platform/brand
    overrides: clark's native mac defaults are already coherent (real GPU, real
    system fonts, mac UA). Verified coherent in the probe.
  - non-darwin: unchanged Windows persona (byte-identical output).
- Golden: add a `system` discriminator to `fork_parity_args` cases (mirrors how
  `default_stealth_args` already carries `system`). Existing Windows outputs stay
  byte-identical; append darwin cases. Regenerate via `just parity-golden` and
  review the diff (sanctioned golden regen). Update reader
  (`parity_test.go`) + generator (`golden_update_test.go`) to pin `systemName`
  per case.

### Backend selection (config)
- Add `config.BackendNative = "native"`.
- Zero-config default becomes platform-derived: darwin -> `native`, else
  `local`. Inject a built-in `native` context in `LoadFrom`; `Active`'s base
  name = `defaultContextName()`.
- Explicit `--context local` still forces Docker on mac (coexist). `--context
  native` on non-darwin errors clearly (guarded in the backend).

### Native backend (internal/backend/native.go)
A local process supervisor (no Docker). Mirrors the `Backend` interface:
- `Start`: resolve the clark binary (below), spawn `cuttle serve
  --headless=false --port=<cdp> [--proxy] [--idle-timeout] [--keep-profile]
  -- --window-size=1280,800` as a detached background process
  (`Setpgid`, own session), env `CUTTLE_BROWSER_BINARY=<clark>`. Redirect
  stdout/stderr to a log under the state dir. Record pid + port in a small state
  file. `os.Executable()` is the cuttle binary to re-exec.
- `State`: read the state file; process alive + CDP answers -> running.
- `Stop`: SIGTERM the serve pgid (serve drains Chrome on SIGTERM); purge removes
  the profile dir.
- `Reach`: loopback `CDPPort`, `VNCPort: 0` (no VNC).
- `check`: error on non-darwin.
- State dir: `<XDG_STATE or ~/.local/state>/cuttle/native/<name>/` holding
  `state.json` (pid, port) + `serve.log`.

### Binary provisioning (internal/backend/nativebin.go or fingerprint)
`EnsureNativeBinary()`:
1. `CUTTLE_BROWSER_BINARY` set -> use it (escape hatch).
2. else cache `<XDG_CACHE or ~/.cache>/cuttle/clark/<tag>/Chromium.app/Contents/MacOS/Chromium`
   present -> use it.
3. else download the pinned release asset, verify sha256, extract, `xattr -dr
   com.apple.quarantine`, return path.

Pinned constants (match Dockerfile provenance):
- tag `chromium-v148.0.7778.96-stealth5`
- asset `clark-browser-darwin-arm64.tar.gz`
- sha256 `c3f16e23262d16d8f899414143dadf06f326fa29ab9d24006d28a68cf5fe3040`

### Handoff: `cuttle view [seed]` (native window surface)
- Ensure serve up + trigger the seed launch (GET
  `/json/version?fingerprint=<seed>` makes the pool spawn that Chrome).
- Raise that seed's window: find the browser PID by matching its
  `--user-data-dir` (pgrep, excluding `--type=` helpers), then
  `osascript` `set frontmost of (first process whose unix id is <pid>)`. Fall
  back to activating the Chromium app.
- Briefing/login: on native (VNCPort 0), print "browser window is on your
  desktop; run `cuttle view` to raise it" instead of a viewer URL. `cuttle
  login` opens the window via `cuttle view` rather than a viewer URL.

## Resolved during implementation
- **Keychain**: added `--use-mock-keychain` so Chromium never prompts for macOS
  Keychain access on launch (os_crypt Safe Storage). Cookies still persist; not a
  web-visible surface. Decided to keep mock (portability + UX); native Keychain
  would only add stronger at-rest encryption of saved logins.
- **Per-seed window targeting (fixed properly)**: the first cut used `pgrep -f
  "--user-data-dir=..."`, which is wrong twice - the leading `--` is parsed as a
  pgrep flag, and the user-data-dir sits at the tail of a long argv (truncation-
  prone). Verified on a live instance that serve launches each seed's Chrome as a
  direct child, so `pgrep -P <servePID>` yields exactly the browser mains (one
  per seed), and `ps -p <pid> -o command=` prints the full untruncated argv for
  disambiguation by seed dir. Precise raise via System Events (needs one-time
  Automation permission); falls back to `open -a` app activation otherwise.
- `#273` window/viewport desync: `--window-size=1280,800` in the native launch.
- Windows golden output kept byte-identical (tripwire intact); macOS persona
  added as new cases with a `system` discriminator.

## Verified (smoke on M1 Max, XDG redirected to scratch)
- `cuttle up` on darwin auto-selects native, downloads+verifies clark, spawns the
  detached daemon, CDP answers (Chrome/148), persona = macOS
  (`--fingerprint-platform=macos`, `Intel Mac OS X 10_15_7` UA).
- `cuttle view` / `cuttle view <seed>` raise the exact seed window.
- `cuttle down --purge` stops the daemon and cleans up (no leftover processes).
- go build, golangci-lint (0), gofumpt, go test ./... (177 pass).

## Phase order
A. Fingerprint macOS persona + golden regen.
B. config: BackendNative + darwin default.
C. backend/native.go supervisor.
D. binary provisioning.
E. backend.New dispatch + resolve() + briefing/login/view.
F. `cuttle view` window surface.
G. build + lint + test + smoke on this Mac.
