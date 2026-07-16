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

import cloakbrowser.config as cfg
import cloakbrowser.geoip as geoip_mod
from cloakbrowser.browser import (
    _ensure_proxy_scheme,
    _reconstruct_socks_url,
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


def _compose_argv(seed, proxy, timezone, locale, webrtc):
    """Mirror the multiplexer's fingerprint arg assembly using only the vendored
    primitives, so the full argv exercises proxy + WebRTC within build_args."""
    extra = []
    if seed is not None:
        extra.append(f"--fingerprint={seed}")
    if proxy is not None:
        extra.append(f"--proxy-server={_normalize_socks_string_url(proxy)}")
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

    out["reconstruct_socks"] = [
        {
            "server": server,
            "username": username,
            "password": password,
            "output": _reconstruct_socks_url(
                {"server": server, "username": username, "password": password}
            ),
        }
        for server, username, password in [
            ("socks5://proxy.example:1080", "", ""),
            ("socks5://proxy.example:1080", "user", "pass"),
            ("socks5://proxy.example:1080", "user", ""),
            ("socks5://proxy.example:1080", "u@s=r", "p@ss=word"),
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
