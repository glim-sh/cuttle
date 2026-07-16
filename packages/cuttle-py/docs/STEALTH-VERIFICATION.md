# Verifying the stealth identity

How to confirm a running cuttle seed presents a coherent, healthy browser
identity - and how to read the noise Chrome prints on the way there. This is the
"is the browser actually OK?" checklist; `test/harness.py` automates the
per-seed isolation and coherence checks, this doc covers what a good identity
looks like and the gotchas that look alarming but aren't.

## Probe a live seed

Point any CDP client at a seed and evaluate these in a page. The values are
seed-derived, so exact strings vary; what matters is that each is *coherent* with
the platform the seed claims (Windows by default).

```js
// navigator surface
navigator.platform            // "Win32"  (coherent with the Windows UA)
navigator.webdriver           // false
navigator.userAgent           // "...Windows NT 10.0... Chrome/148..."  (no "HeadlessChrome")
navigator.hardwareConcurrency // seed-derived small even number (4/6/8/12/16)

// WebGL GPU (the load-bearing one)
const gl = document.createElement('canvas').getContext('webgl');
const dbg = gl.getExtension('WEBGL_debug_renderer_info');
gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL)    // e.g. "Google Inc. (NVIDIA)"
gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL)  // e.g. "ANGLE (NVIDIA, NVIDIA GeForce RTX 3060 ... Direct3D11 ...)"
```

Expected, on a healthy seed:

| Surface | Good | Bad (investigate) |
|---|---|---|
| `navigator.webdriver` | `false` | `true` |
| `navigator.platform` | matches the UA platform (`Win32`) | mismatched (e.g. `Linux` under a Windows UA) |
| WebGL renderer | a real desktop GPU string via **ANGLE / Direct3D11** | contains `SwiftShader`, `llvmpipe`, or `Mesa` |
| WebRTC ICE candidates | only the proxy exit IP, or none | a private/LAN IP (`10.*`, `192.168.*`, `172.16-31.*`) or the host IP |
| WebGPU (`navigator.gpu`) | absent, or an adapter matching the WebGL GPU | an adapter that contradicts the WebGL GPU |

The fork spoofs the WebGL GPU strings from a pool of real desktop GPUs, so the
renderer reads as a genuine ANGLE/Direct3D11 adapter **even though the host has
no GPU**. If WebGL ever reports `SwiftShader`/`llvmpipe`, the spoof is not
engaging - that is a real regression.

## Chrome log lines that are benign

Chrome writes a lot of stderr in a headless-server container. These are noise -
they do not indicate a broken browser or a leaking identity:

| Log line | Why it's harmless |
|---|---|
| `Failed to connect to the bus` (dbus) | No system D-Bus in a container; Chrome logs and continues. |
| `Failed to adjust OOM score of renderer ... Permission denied` | Needs a capability the container doesn't grant; cosmetic. |
| `Failed to decode OID` (`ev_root_ca_metadata`) | Cert-metadata warning, unrelated to stealth. |
| `vkCreateInstance: Found no drivers` / `VK_ERROR_INCOMPATIBLE_DRIVER` | No Vulkan GPU on the host; expected. |
| `Automatic fallback to software WebGL ... --enable-unsafe-swiftshader` | See below - do **not** act on this one. |
| `GPU stall due to ReadPixels` | Performance note, not a correctness issue. |

## Do not add `--enable-unsafe-swiftshader`

Chrome nudges you to pass `--enable-unsafe-swiftshader` when WebGL falls back to
software rendering. **Passing it is a stealth regression, not a fix.** It forces
the SwiftShader software renderer, and a raw `SwiftShader`/`llvmpipe` WebGL
string is a well-known automation tell. The fork instead spoofs a real GPU string
on top of whatever renders underneath, so the coherent ANGLE/Direct3D11 identity
is what a detector sees. `--ignore-gpu-blocklist` (already in the base args) is
what lets WebGL work at all under software rendering; the fork's patches make it
*look* real. Upstream `cloakserve` reaches the same conclusion and actively
suppresses this flag for the same reason.

## Challenge cold-clear depends on the exit IP, not the fingerprint

When a seed first loads a site behind an escalated anti-bot challenge, whether it
clears is dominated by the **reputation of that seed's proxy exit IP**, not by the
browser fingerprint. A coherent identity on a clean exit clears in seconds; the
same identity on a flagged exit will not clear no matter how many times the page
is re-loaded on that same IP.

The operational consequence for a CDP client: on a *persistent* challenge, rotate
to a fresh seed (which draws a new proxy exit) rather than retrying the same seed.
Re-loading the same seed burns time against a losing IP; a fresh exit is what
actually clears. Budget one cheap same-exit retry for a transient challenge, then
rotate.
