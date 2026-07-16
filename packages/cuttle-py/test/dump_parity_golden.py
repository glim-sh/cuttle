"""Dump fingerprint parity goldens from the vendored cloakbrowser oracle.

The Go port (packages/cuttle-go/internal/fingerprint) must reproduce this output
byte-for-byte across the input matrix; a silent drift is a silent stealth loss.

Hermeticity: platform is pinned to Linux (the container target), the stealth seed
is pinned, and the proxy exit-IP resolver is stubbed to a fixed documentation IP,
so no live network or host-platform state leaks into the goldens.

Run via `uv run python test/dump_parity_golden.py` (or `just parity-golden` in
packages/cuttle-go). Writes the golden JSON into the Go testdata dir.
"""

from __future__ import annotations

import json
import platform
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

import cloakbrowser.config as cfg
import cloakbrowser.geoip as geoip_mod
from cloakbrowser.browser import (
    _ensure_proxy_scheme,
    _normalize_socks_string_url,
    _resolve_webrtc_args,
    build_args,
)

PINNED_SEED = 55555
STUB_EXIT_IP = "203.0.113.7"  # RFC 5737 TEST-NET-3 documentation address

GOLDEN_PATH = (
    Path(__file__).resolve().parents[2]
    / "cuttle-go/internal/fingerprint/testdata/golden.json"
)

# Proxy matrix: absent / http / socks5 / with-auth (http + socks) / raw special
# characters that force credential re-encoding.
PROXIES = [
    None,
    "http://proxy.example:8080",
    "socks5://proxy.example:1080",
    "http://user:p%40ss@proxy.example:8080",
    "socks5://user:p%40ss@proxy.example:1080",
    "socks5://user:p@ss=word@proxy.example:1080",
]
SEEDS = [None, "12345"]
TZLOCALES = [(None, None), ("America/New_York", "en-US")]
WEBRTCS = ["none", "auto"]


def _pin_platform(system: str) -> None:
    platform.system = lambda: system  # type: ignore[assignment]


# Copied verbatim from bin/cuttleserve._split_proxy_auth (which cannot be
# imported here without pulling in aiohttp/websockets). The Go port
# fingerprint.SplitProxyAuth must reproduce this byte-for-byte.
def _split_proxy_auth(proxy):
    parts = urlsplit(proxy)
    if parts.scheme not in ("http", "https") or parts.username is None:
        return proxy, None, None
    host = parts.hostname or ""
    if parts.port:
        host = f"{host}:{parts.port}"
    stripped = urlunsplit((parts.scheme, host, parts.path, parts.query, parts.fragment))
    return stripped, parts.username, parts.password or ""


# Copied verbatim from bin/cuttleserve's fork-parity block (guarded there by
# CLOAKBROWSER_BINARY_PATH). The Go port fingerprint.ForkParityArgs must match.
def _fork_parity_args(locale, proxy):
    lang = locale or "en-US"
    base = lang.split("-", 1)[0]
    args = [
        "--fingerprint-platform=windows",
        "--fingerprint-platform-version=19.0.0",
        "--fingerprint-brand=Chrome",
        "--fingerprint-brand-version=148.0.0.0",
        "--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36",
        "--fingerprint-fonts-dir=/opt/winfonts",
        "--fingerprinting-client-rects-noise",
        "--fingerprinting-canvas-measuretext-noise",
        "--fingerprinting-canvas-image-data-noise",
        f"--accept-lang={lang},{base}" if base != lang else f"--accept-lang={lang}",
    ]
    if proxy:
        args.append("--fingerprint-network-profile=residential")
    return args


def _compose_argv(seed, proxy, timezone, locale, webrtc):
    """Mirror the multiplexer's fingerprint arg assembly using only the vendored
    primitives, so the full argv exercises proxy + WebRTC within build_args. The
    proxy is credential-stripped (as cuttleserve does) before normalization."""
    extra = []
    if seed is not None:
        extra.append(f"--fingerprint={seed}")
    if proxy is not None:
        stripped, _, _ = _split_proxy_auth(proxy)
        extra.append(f"--proxy-server={_normalize_socks_string_url(stripped)}")
    if webrtc == "auto":
        extra.append("--fingerprint-webrtc-ip=auto")
    extra = _resolve_webrtc_args(extra, proxy)
    return build_args(True, extra, timezone=timezone, locale=locale, headless=True)


def main() -> None:
    # Pin randomness and the exit-IP resolver for all sections.
    cfg.random.randint = lambda a, b: PINNED_SEED  # type: ignore[assignment]
    geoip_mod.resolve_proxy_exit_ip = lambda url: STUB_EXIT_IP  # type: ignore[assignment]

    out: dict = {}
    out["exit_ip_stub"] = STUB_EXIT_IP
    out["country_locale_map"] = dict(geoip_mod.COUNTRY_LOCALE_MAP)

    default_stealth = []
    for system in ("Linux", "Darwin", "Windows"):
        _pin_platform(system)
        default_stealth.append(
            {
                "system": system,
                "seed": PINNED_SEED,
                "output": cfg.get_default_stealth_args(),
            }
        )
    out["default_stealth_args"] = default_stealth

    # Everything below is dumped with the container target platform.
    _pin_platform("Linux")

    out["ensure_proxy_scheme"] = [
        {"input": s, "output": _ensure_proxy_scheme(s)}
        for s in [
            "proxy.example:8080",
            "http://proxy.example:8080",
            "socks5://proxy.example:1080",
        ]
    ]

    out["normalize_socks"] = [
        {"input": p, "output": _normalize_socks_string_url(p)}
        for p in [
            "http://proxy.example:8080",
            "socks5://proxy.example:1080",
            "http://user:p%40ss@proxy.example:8080",
            "socks5://user:p%40ss@proxy.example:1080",
            "socks5://user:p@ss=word@proxy.example:1080",
            "socks5://USER:P@SS@HOST.example:1080",
            "socks5://user:@proxy.example:1080",
            "socks5://user@proxy.example:1080",
            "socks5://user:pass@[2001:db8::1]:1080",
            "socks5://proxy.example:not-a-port",
        ]
    ]


    out["split_proxy_auth"] = []
    for p in [
        "http://bob:secret@proxy.example:8080",
        "http://bob:secret@Proxy.EXAMPLE:8080",
        "http://user%40host:p%40ss@host.example:8080",
        "https://u:p@[2001:db8::1]:8443",
        "http://bob@proxy.example:8080",
        "http://user:@proxy.example:8080",
        "http://proxy.example:8080",
        "socks5://user:pass@proxy.example:1080",
    ]:
        server, user, password = _split_proxy_auth(p)
        out["split_proxy_auth"].append(
            {"input": p, "server": server, "username": user or "", "password": password or ""}
        )

    out["fork_parity_args"] = [
        {"locale": loc, "proxy": px, "output": _fork_parity_args(loc, px)}
        for loc, px in [
            ("", None),
            ("en-US", None),
            ("de-DE", "http://p:1"),
            ("fr", "socks5://p:1"),
            ("", "http://p:1"),
        ]
    ]

    webrtc_cases = []
    for name, args, proxy, stub in [
        ("no-auto", ["--fingerprint=1", "--no-sandbox"], "http://proxy.example:8080", STUB_EXIT_IP),
        ("auto-http", ["--fingerprint-webrtc-ip=auto"], "http://proxy.example:8080", STUB_EXIT_IP),
        ("auto-socks", ["--fingerprint-webrtc-ip=auto"], "socks5://proxy.example:1080", STUB_EXIT_IP),
        ("auto-no-proxy", ["--fingerprint-webrtc-ip=auto"], None, STUB_EXIT_IP),
        ("auto-empty-ip", ["--x", "--fingerprint-webrtc-ip=auto"], "http://proxy.example:8080", None),
    ]:
        geoip_mod.resolve_proxy_exit_ip = lambda url, _s=stub: _s  # type: ignore[assignment]
        webrtc_cases.append(
            {
                "input_args": args,
                "proxy": proxy,
                "exit_ip": stub,
                "output": _resolve_webrtc_args(list(args), proxy),
            }
        )
    geoip_mod.resolve_proxy_exit_ip = lambda url: STUB_EXIT_IP  # type: ignore[assignment]
    out["resolve_webrtc"] = webrtc_cases

    # build_args-only cases: headless/extension/start_maximized/Windows coverage.
    build_cases = []
    for name, kwargs in [
        ("headed-adds-gpu-flag", {"stealth_args": True, "extra_args": ["--fingerprint=1"], "headless": False}),
        ("timezone-only", {"stealth_args": True, "extra_args": None, "timezone": "Europe/Berlin"}),
        ("locale-only", {"stealth_args": True, "extra_args": None, "locale": "de-DE"}),
        ("no-stealth", {"stealth_args": False, "extra_args": ["--foo=bar"], "timezone": "UTC", "locale": "en-US"}),
        ("override-fingerprint", {"stealth_args": True, "extra_args": ["--fingerprint=99999", "--proxy-server=http://h:1"]}),
        (
            "extensions",
            {"stealth_args": True, "extra_args": ["--fingerprint=1"], "extension_paths": ["/opt/ext/a", "/opt/ext/b"]},
        ),
        (
            "start-maximized",
            {"stealth_args": True, "extra_args": ["--fingerprint=1"], "start_maximized": True},
        ),
        (
            "start-maximized-suppressed",
            {"stealth_args": True, "extra_args": ["--fingerprint=1", "--window-size=800,600"], "start_maximized": True},
        ),
    ]:
        build_cases.append({"name": name, "input": kwargs, "output": build_args(**kwargs)})
    out["build_args"] = build_cases

    compose = []
    for seed in SEEDS:
        for proxy in PROXIES:
            for timezone, locale in TZLOCALES:
                for webrtc in WEBRTCS:
                    compose.append(
                        {
                            "seed": seed,
                            "proxy": proxy,
                            "timezone": timezone,
                            "locale": locale,
                            "webrtc": webrtc,
                            "output": _compose_argv(seed, proxy, timezone, locale, webrtc),
                        }
                    )
    out["compose_argv"] = compose

    GOLDEN_PATH.parent.mkdir(parents=True, exist_ok=True)
    GOLDEN_PATH.write_text(json.dumps(out, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"wrote {GOLDEN_PATH} ({len(compose)} compose_argv entries)")


if __name__ == "__main__":
    main()
