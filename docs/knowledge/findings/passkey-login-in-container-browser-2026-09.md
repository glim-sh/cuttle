---
type: Finding
title: Passkey login in the container browser, September 2026 - what is reachable
description: Snapshot of a 2026-09-29 survey and probe of whether a passkey held on the operator's host can sign into sites inside cuttle's container browser; host authenticators are unreachable, a vault extension or a CDP virtual authenticator are the workable paths, and stock 154 reports no platform authenticator and no hybrid transport on either desktop persona.
tags: [webauthn, passkey, auth, stealth, fingerprint, survey]
status: stable
stale_after: "2027-03-01T00:00:00+00:00"
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T18:30:00+00:00" }
sources:
  - id: code
    resource: "cuttle source at feat/chromium-154-rebase on 2026-09-29: packages/cuttle/internal/backend/local.go (docker run args), internal/fingerprint/args.go:135,190-202,313-318, internal/serve/wsproxy.go:868-898, internal/serve/secretfill.go, internal/cli/SKILL.md:191, packages/browser/patches, ops/docker/Dockerfile"
    title: cuttle code survey
  - id: probe
    resource: "CDP probe through cuttle serve of the final 154.0.8037.57 images (macOS persona on arm64, Windows persona on amd64) on 2026-09-29 (session record): PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable() and getClientCapabilities(), stock and after WebAuthn.addVirtualAuthenticator"
    title: Probe of our own build
  - id: cable
    resource: https://chromium.googlesource.com/chromium/src/+/HEAD/device/fido/cable/fido_cable_discovery.cc
    title: Chromium fido_cable_discovery.cc - no BLE adapter, no hybrid discovery
  - id: reqhandler
    resource: https://chromium.googlesource.com/chromium/src/+/08b868b07cdfae0413395a502546cc51eadf9029/device/fido/fido_request_handler_base.cc
    title: Chromium fido_request_handler_base.cc - hybrid transport erased when no adapter is present
  - id: loadext
    resource: https://groups.google.com/a/chromium.org/g/chromium-extensions/c/1-g8EFx2BBY
    title: Chromium extensions PSA - --load-extension removal applies to branded Chrome only
  - id: bw
    resource: https://bitwarden.com/help/storing-passkeys.md
    title: Bitwarden - storing and using passkeys
  - id: vw
    resource: https://github.com/dani-garcia/vaultwarden/pull/4025
    title: Vaultwarden PR #4025 - passkey (fido2-vault-credentials) support
  - id: bwexport
    resource: https://sdk-api-docs.bitwarden.com/bitwarden_exporters/json/struct.JsonFido2Credential.html
    title: Bitwarden JSON export - fido2 credential with PKCS#8 key value
  - id: cxf
    resource: https://fidoalliance.org/specs/cx/cxf-v1.0-ps-20250814.html
    title: FIDO Credential Exchange Format v1.0 - passkey key as PKCS#8
  - id: cdpwebauthn
    resource: https://chromedevtools.github.io/devtools-protocol/tot/WebAuthn/
    title: CDP WebAuthn domain
  - id: cloak147
    resource: https://github.com/CloakHQ/CloakBrowser/issues/147
    title: CloakBrowser #147 - getClientCapabilities hybrid/platform flags as a container tell
  - id: cloak176
    resource: https://github.com/CloakHQ/CloakBrowser/issues/176
    title: CloakBrowser #176 - virtual authenticator enrollment rejected over a zero AAGUID; transport internal
  - id: cloak505
    resource: https://github.com/CloakHQ/CloakBrowser/issues/505
    title: CloakBrowser #505 - webAuthenticationProxy blocked by ungoogled domain substitution
  - id: ug3914
    resource: https://github.com/ungoogled-software/ungoogled-chromium/pull/3914
    title: ungoogled-chromium PR #3914 - domain-substitution exception declined
  - id: camoufox718
    resource: https://github.com/daijro/camoufox/issues/718
    title: Camoufox #718 - platform authenticator always false on desktop personas
  - id: bbauth
    resource: https://docs.browserbase.com/platform/identity/authentication
    title: Browserbase authentication docs - virtual authenticator to suppress passkey prompts
  - id: skyvern
    resource: https://github.com/Skyvern-AI/skyvern/pull/7877
    title: Skyvern PR #7877 - passkey as a 2FA credential type
  - id: wap
    resource: https://developer.chrome.com/docs/extensions/reference/api/webAuthenticationProxy
    title: chrome.webAuthenticationProxy extension API
  - id: rdpolicy
    resource: https://chromeenterprise.google/policies/web-authentication-remote-desktop-allowed-origins
    title: WebAuthenticationRemoteDesktopAllowedOrigins policy
  - id: apple
    resource: https://developer.apple.com/documentation/authenticationservices/asauthorizationwebbrowserpublickeycredentialmanager
    title: Apple web-browser public-key-credential API and entitlement
  - id: kasm
    resource: https://docs.kasm.com/docs/develop/how-to/security/users-groups-mgmt/groups
    title: Kasm group settings - WebAuthn passthrough is RDP with mstsc.exe only
  - id: neko
    resource: https://github.com/m1k1o/neko/issues/304
    title: neko #304 - WebAuthn closed not planned
  - id: orb698
    resource: https://github.com/orbstack/orbstack/issues/698
    title: OrbStack #698 - USB passthrough unsupported
---

# Passkey login in the container browser, September 2026

A snapshot taken on 2026-09-29, against the final Chromium 154.0.8037.57 images.

## Our setup

- **The browser can't see anything on the host.** It is Linux Chromium in a Docker container. The container gets no `--device`, no USB, no Bluetooth and no dbus socket; the only mounts are the profile volume and shm. So macOS Touch ID, iCloud Keychain and a USB security key on the host are all invisible to it.[^code]
- **A human in the VNC viewer can't help.** The WebAuthn prompt comes from the container, so it can't reach the host's authenticators either.[^code]
- **None of our browser patches touch WebAuthn.** No patch, flag or daemon code references it.[^code]
- **The daemon passes the CDP `WebAuthn.*` domain through.** It blocks only `Target.createBrowserContext`.[^code]
- **Extensions can't be loaded today.** The base args force `--disable-extensions`. `ExtensionPaths` builds `--load-extension`, but nothing in the daemon sets it.[^code]
- **Secrets are text only.** The daemon-owned secrets are text values for input fields and can't carry an authenticator key.[^code]
- **The skill hands passkeys to a human.** It tells agents to hand off a passkey or hardware-key prompt.[^code]

## Probe of our own build

These are the answers the page sees through `cuttle serve`. The macOS and Windows personas gave identical values.[^probe]

| Signal | Stock | After `addVirtualAuthenticator` (ctap2, `transport:"internal"`, resident key, UV) |
|---|---|---|
| `isUserVerifyingPlatformAuthenticatorAvailable()` | false | true |
| `getClientCapabilities().passkeyPlatformAuthenticator` | false | true |
| `getClientCapabilities().userVerifyingPlatformAuthenticator` | false | true |
| `getClientCapabilities().hybridTransport` | false | false |
| `isConditionalMediationAvailable()`, `conditionalGet`, `conditionalCreate` | true | true |

- **A missing platform authenticator is a known container tell.** CloakBrowser reports `hybridTransport` and `passkeyPlatformAuthenticator` as `true` on real desktop machines and `false` under docker/xvfb, and patches both.[^cloak147] Camoufox tracks the same gap.[^camoufox718]
- **`isUserVerifyingPlatformAuthenticatorAvailable` is noisier.** Real Windows machines without Hello set up return `false`.[^cloak147]
- **We have not measured these values in real Chrome 154 ourselves.**

## Paths to a passkey login

| Path | Status in this setup | Constraints |
|---|---|---|
| Passkey-provider extension (Bitwarden, or self-hosted Vaultwarden) in the container browser | Workable | Unbranded Chromium still honours `--load-extension`.[^loadext] The extension overrides `navigator.credentials` in the page and serves passkeys synced from the vault.[^bw] Vaultwarden needs `fido2-vault-credentials`.[^vw] Needs cuttle to allow extensions, and adds an extension to the fingerprint. |
| CDP virtual authenticator (`WebAuthn.addVirtualAuthenticator` + `addCredential`) | Workable | It can mint its own keys, or load exported ones. Injected keys must be ES256/P-256 PKCS#8.[^cdpwebauthn] Bitwarden JSON and FIDO CXF exports carry the key as PKCS#8.[^bwexport][^cxf] iCloud Keychain and hardware-key private keys can't be exported. A zero AAGUID was rejected at enrollment by a large identity provider; `transport:"internal"` keeps Chromium's test AAGUID, which a relying party that checks FIDO metadata can still reject.[^cloak176] |
| Relay to a host authenticator via a `chrome.webAuthenticationProxy` extension | Possible, not built anywhere found | The API intercepts WebAuthn in the "remote" browser.[^wap] The host side needs a helper that drives a local authenticator. iCloud Keychain needs Apple's browser entitlement, granted only to approved browsers.[^apple] ungoogled's domain substitution breaks the proxy for the AppID-checked origins of one large identity provider,[^cloak505] and the upstream fix was declined.[^ug3914] Chromium's built-in `remoteDesktopClientOverride` is the client-side half and needs a managed-profile policy.[^rdpolicy] |
| Hybrid ("use a phone or tablet" QR) | Not reachable | Chromium drops the hybrid transport when no BLE adapter is present,[^cable][^reqhandler] and the container has none. |
| USB security key passthrough into Docker on macOS | Not reachable | Docker VMs on macOS don't expose host USB HID devices.[^orb698] |
| KasmVNC or neko passthrough | Does not exist | Kasm's WebAuthn passthrough is RDP-only, from Windows `mstsc.exe` clients.[^kasm] neko closed its WebAuthn request as not planned.[^neko] |

## What peers do

- **No surveyed project bridges a host passkey into a container browser.**
- **Browserbase** attaches a CDP virtual authenticator so passkey prompts never appear.[^bbauth]
- **Skyvern** stores passkeys as a 2FA credential type. Its injection mechanism is not public.[^skyvern]
- **CloakBrowser** spoofs the capability answers[^cloak147] and documents the virtual-authenticator configuration.[^cloak176]

[^code]: cuttle code survey
[^probe]: Probe of our own build
[^cable]: Chromium fido_cable_discovery.cc - no BLE adapter, no hybrid discovery
[^reqhandler]: Chromium fido_request_handler_base.cc - hybrid transport erased when no adapter is present
[^loadext]: Chromium extensions PSA - --load-extension removal applies to branded Chrome only
[^bw]: Bitwarden - storing and using passkeys
[^vw]: Vaultwarden PR #4025 - passkey (fido2-vault-credentials) support
[^bwexport]: Bitwarden JSON export - fido2 credential with PKCS#8 key value
[^cxf]: FIDO Credential Exchange Format v1.0 - passkey key as PKCS#8
[^cdpwebauthn]: CDP WebAuthn domain
[^cloak147]: CloakBrowser #147 - getClientCapabilities hybrid/platform flags as a container tell
[^cloak176]: CloakBrowser #176 - virtual authenticator enrollment rejected over a zero AAGUID; transport internal
[^cloak505]: CloakBrowser #505 - webAuthenticationProxy blocked by ungoogled domain substitution
[^ug3914]: ungoogled-chromium PR #3914 - domain-substitution exception declined
[^camoufox718]: Camoufox #718 - platform authenticator always false on desktop personas
[^bbauth]: Browserbase authentication docs - virtual authenticator to suppress passkey prompts
[^skyvern]: Skyvern PR #7877 - passkey as a 2FA credential type
[^wap]: chrome.webAuthenticationProxy extension API
[^rdpolicy]: WebAuthenticationRemoteDesktopAllowedOrigins policy
[^apple]: Apple web-browser public-key-credential API and entitlement
[^kasm]: Kasm group settings - WebAuthn passthrough is RDP with mstsc.exe only
[^neko]: neko #304 - WebAuthn closed not planned
[^orb698]: OrbStack #698 - USB passthrough unsupported
