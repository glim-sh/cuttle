"""Stealth configuration and platform detection for cuttle."""

from __future__ import annotations

import os
import platform
import random
import re
from pathlib import Path


IGNORE_DEFAULT_ARGS = ["--enable-automation", "--enable-unsafe-swiftshader"]


def get_default_stealth_args() -> list[str]:
    """Build stealth args with a random fingerprint seed per launch.

    On macOS, skips platform/GPU spoofing — runs as a native Mac browser.
    Spoofing Windows on Mac creates detectable mismatches (fonts, GPU, etc.).
    """
    seed = random.randint(10000, 99999)
    system = platform.system()

    base = [
        "--no-sandbox",
        f"--fingerprint={seed}",
    ]

    if system == "Darwin":
        # Tell the fingerprint patches we're on macOS so GPU/UA match natively
        return base + ["--fingerprint-platform=macos"]

    # Linux/Windows: Windows fingerprint profile.
    # Screen and window size come from the real display, not this flag (verified:
    # identical across seeds), so the wrapper must not emulate a viewport on top in
    # headed mode — that would break outerWidth >= innerWidth coherence.
    return base + ["--fingerprint-platform=windows"]


DEFAULT_VIEWPORT = {"width": 1920, "height": 947}


_VERSION_PIN_RE = re.compile(r"^[0-9]+(?:\.[0-9]+){3,4}$")


def normalize_requested_version(version: str | None = None) -> str | None:
    """Return an explicit Chromium version pin from arg/env, or None.

    The explicit argument wins over CLOAKBROWSER_VERSION. Only numeric dotted
    versions are accepted because the value is interpolated into cache paths and
    download URLs.
    """
    raw = version if version is not None else os.environ.get("CLOAKBROWSER_VERSION")
    if raw is None:
        return None
    normalized = raw.strip()
    if not normalized:
        return None
    if not _VERSION_PIN_RE.fullmatch(normalized):
        raise ValueError(
            "Invalid browser version pin. Use a full numeric Chromium version, "
            "e.g. '148.0.7778.215.2'."
        )
    return normalized


def get_cache_dir() -> Path:
    """Return the cache directory for downloaded binaries.

    Override with CLOAKBROWSER_CACHE_DIR env var.
    Default: ~/.cloakbrowser/
    """
    custom = os.environ.get("CLOAKBROWSER_CACHE_DIR")
    if custom:
        return Path(custom)
    return Path.home() / ".cloakbrowser"


def get_local_binary_override() -> str | None:
    """Check if user has set a local binary path via env var.

    Set CLOAKBROWSER_BINARY_PATH to use a locally built Chromium instead of downloading.
    """
    return os.environ.get("CLOAKBROWSER_BINARY_PATH")
