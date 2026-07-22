# syntax=docker/dockerfile:1
# cuttle: a stealth-Chromium CDP farm.
#
# The CDP multiplexer (`cuttle serve`) spawns one stealth Chrome per fingerprint
# seed, routing per-seed identity (fingerprint, proxy, geoip) over CDP. The
# Chrome engine is a FREE, redistributable stealth-Chromium fork (clark, MIT,
# default; clearcote, BSD-3, fallback - currently not baked, see the commented
# stage) baked in as a prebuilt binary and selected via CUTTLE_BROWSER_BINARY.
# No proprietary binary.
#
# The daemon is a single static Go binary and the whole build is Python-free.
# linux/amd64 only: clark/clearcote ship linux-x64 prebuilts. On
# an Apple Silicon host the image runs emulated (fine for local dev + login
# handoff); production runs it native on an amd64 server.

# --- Go builder: the static `cuttle` binary. -------------------------------
FROM golang:1.26-trixie AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath \
      -ldflags="-s -w -X github.com/glim-sh/cuttle/internal/cli.version=${VERSION}" \
      -o /out/cuttle ./cmd/cuttle

# --- Browser engine download stages. ---------------------------------------
# Each engine lives in its own stage so its cache is keyed only on its own
# tag+checksum ARGs: it downloads in parallel with the apt/builder stages and an
# apt package bump never re-downloads a ~200MB browser. BuildKit fetches the URL
# itself (no curl in the stage) and verifies the pinned sha256 via --checksum.

# clark-browser (MIT): ungoogled-chromium 148 + --fingerprint-* patch series,
# the same flag dialect cuttle serve emits. Primary engine.
FROM debian:trixie-slim AS clark
ARG CLARK_TAG=chromium-v148.0.7778.96-stealth5
ARG CLARK_ASSET=clark-browser-linux-x64.tar.gz
ARG CLARK_SHA256=30cca952d11d94ca3424ac184b100c88ba686bfb87f2aaf4668ac5767562bd67
ADD --checksum=sha256:${CLARK_SHA256} \
    https://github.com/clark-labs-inc/clark-browser/releases/download/${CLARK_TAG}/${CLARK_ASSET} /tmp/clark.tgz
RUN mkdir -p /opt/clark \
    && tar -xzf /tmp/clark.tgz -C /opt/clark \
    && rm /tmp/clark.tgz \
    && CLARK_BIN="$(find /opt/clark -maxdepth 3 -type f -name chrome | head -1)" \
    && test -n "${CLARK_BIN}" \
    && if [ "${CLARK_BIN}" != /opt/clark/chrome ]; then ln -sf "${CLARK_BIN}" /opt/clark/chrome; fi \
    && chmod +x "${CLARK_BIN}" \
    && echo "clark chrome -> ${CLARK_BIN}"

# clearcote-browser (BSD-3): Chromium 149 + --fingerprint-* patch series.
# Fallback engine, currently not baked into the image - uncomment this stage and
# its COPY in the runtime stage to re-enable. NOTE: its timezone flag is
# --timezone (not --fingerprint-timezone); cuttle serve special-cases that.
# Ships .tar.xz (xz-utils stays in the runtime apt list for this).
# FROM debian:trixie-slim AS clearcote
# ARG CLEARCOTE_TAG=v0.1.0-pre.18
# ARG CLEARCOTE_ASSET=clearcote-149.0.7827.114-linux-x64.tar.xz
# ARG CLEARCOTE_SHA256=fd96497e921b4fc9f384a5c1377896c8ee7e8a3a1991835c0256b010811e97aa
# ADD --checksum=sha256:${CLEARCOTE_SHA256} \
#     https://github.com/clearcotelabs/clearcote-browser/releases/download/${CLEARCOTE_TAG}/${CLEARCOTE_ASSET} /tmp/clearcote.txz
# RUN mkdir -p /opt/clearcote \
#     && tar -xJf /tmp/clearcote.txz -C /opt/clearcote \
#     && rm /tmp/clearcote.txz \
#     && CLEARCOTE_BIN="$(find /opt/clearcote -maxdepth 3 -type f -name chrome | head -1)" \
#     && test -n "${CLEARCOTE_BIN}" \
#     && if [ "${CLEARCOTE_BIN}" != /opt/clearcote/chrome ]; then ln -sf "${CLEARCOTE_BIN}" /opt/clearcote/chrome; fi \
#     && chmod +x "${CLEARCOTE_BIN}" \
#     && echo "clearcote chrome -> ${CLEARCOTE_BIN}"

# --- Font pack builder: Windows-family fonts (discarded from the runtime). --
# A Windows persona must expose Windows font FAMILY NAMES to enumeration probes
# (canvas measureText / CSS matching) while still rendering real glyphs - incl.
# color emoji and CJK, whose pixel output aggressive anti-bot sites (Kasada/
# Akamai) hash and compare against real browsers. So we take free, metric- or
# coverage-compatible fonts and rewrite their `name` table to the Windows family
# they stand in for (Liberation->Arial/Times New Roman/Courier New, Carlito->
# Calibri/Segoe UI, Caladea->Cambria, Noto Color Emoji->Segoe UI Emoji, WenQuanYi
# Zen Hei->Microsoft YaHei, IPAGothic->Yu Gothic/MS Gothic, Loma->Leelawadee UI).
# Renaming needs python3+fonttools, kept entirely in this stage so the runtime
# image stays Python-free. Also builds `cuttle-null`, a glyphless sink font that
# 50-block-linux-aliases.conf points the Linux family names at (see that file).
FROM debian:trixie-slim AS fontpack
RUN apt-get update && apt-get install -y --no-install-recommends \
      python3 python3-fonttools fontconfig \
      fonts-liberation2 fonts-crosextra-carlito fonts-crosextra-caladea \
      fonts-noto-color-emoji fonts-wqy-zenhei fonts-ipafont-gothic fonts-tlwg-loma-otf \
    && rm -rf /var/lib/apt/lists/*
COPY scripts/rename-fonts.py /tmp/rename-fonts.py
COPY scripts/make-null-font.py /tmp/make-null-font.py
RUN set -e; mkdir -p /opt/winfonts; \
    L="$(dirname "$(fc-list | grep -m1 -i LiberationSans-Regular | cut -d: -f1)")"; \
    X="$(dirname "$(fc-list | grep -m1 -i Carlito-Regular | cut -d: -f1)")"; \
    for s in Regular Bold Italic BoldItalic; do \
      python3 /tmp/rename-fonts.py "$L/LiberationSans-$s.ttf"  "Arial"           "/opt/winfonts/arial-$s.ttf"; \
      python3 /tmp/rename-fonts.py "$L/LiberationSerif-$s.ttf" "Times New Roman" "/opt/winfonts/times-$s.ttf"; \
      python3 /tmp/rename-fonts.py "$L/LiberationMono-$s.ttf"  "Courier New"     "/opt/winfonts/cour-$s.ttf"; \
      python3 /tmp/rename-fonts.py "$X/Carlito-$s.ttf"         "Calibri"         "/opt/winfonts/calibri-$s.ttf"; \
      python3 /tmp/rename-fonts.py "$X/Carlito-$s.ttf"         "Segoe UI"        "/opt/winfonts/segoeui-$s.ttf"; \
      python3 /tmp/rename-fonts.py "$X/Caladea-$s.ttf"         "Cambria"         "/opt/winfonts/cambria-$s.ttf"; \
    done; \
    python3 /tmp/rename-fonts.py /usr/share/fonts/truetype/noto/NotoColorEmoji.ttf "Segoe UI Emoji" /opt/winfonts/segoeuiemoji.ttf; \
    python3 /tmp/rename-fonts.py /usr/share/fonts/truetype/wqy/wqy-zenhei.ttc     "Microsoft YaHei" /opt/winfonts/msyahei.ttf --ttc-index 0; \
    python3 /tmp/rename-fonts.py /usr/share/fonts/opentype/ipafont-gothic/ipagp.ttf "Yu Gothic" /opt/winfonts/yugothic.ttf; \
    python3 /tmp/rename-fonts.py /usr/share/fonts/opentype/ipafont-gothic/ipag.ttf  "MS Gothic" /opt/winfonts/msgothic.ttf; \
    for f in Loma:Regular Loma-Bold:Bold Loma-Oblique:Italic Loma-BoldOblique:BoldItalic; do \
      python3 /tmp/rename-fonts.py "/usr/share/fonts/opentype/tlwg/${f%%:*}.otf" "Leelawadee UI" "/opt/winfonts/leelawadeeui-${f##*:}.otf"; \
    done; \
    python3 /tmp/make-null-font.py /opt/winfonts/cuttle-null.ttf; \
    echo "winfonts families:"; fc-scan --format='%{family[0]}\n' /opt/winfonts/* | sort -u

# --- Runtime: Python-free stealth-Chromium farm. ---------------------------
# Base is Debian trixie (same OS as golang:1.26-trixie and python:3.12-slim), so
# the KasmVNC bookworm .deb and the font families match across stages.
FROM debian:trixie-slim

# Chromium system libs + headed-mode stack (Xvfb/openbox: headed Chrome is
# REQUIRED to clear escalated anti-bot challenges) + fontconfig + xz (clearcote
# ships .tar.xz) + X debug tools (xwininfo/xdpyinfo/scrot: verify window
# mapping and grab the :99 display in-container) + bash (the entrypoint script).
# No nodejs, no python (the daemon is a static Go binary). Deliberately NO system
# font packages: every glyph the Windows persona is allowed to expose ships in
# the /opt/winfonts pack (built in the `fontpack` stage), and fontconfig is
# restricted to that dir below - so no Linux family name (Liberation/DejaVu/
# Noto/WenQuanYi/IPAGothic/Loma/Carlito/Caladea) can leak to font-enumeration.
RUN apt-get update && apt-get install -y --no-install-recommends \
      libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libcups2 \
      libdbus-1-3 libdrm2 libxkbcommon0 libatspi2.0-0 libxcomposite1 \
      libxdamage1 libxfixes3 libxrandr2 libgbm1 libpango-1.0-0 \
      libcairo2 libasound2 libx11-xcb1 libfontconfig1 libx11-6 \
      libxcb1 libxext6 libxshmfence1 libglib2.0-0 libgtk-3-0 \
      libpangocairo-1.0-0 libcairo-gobject2 libgdk-pixbuf-2.0-0 \
      libxss1 libxtst6 \
      fontconfig \
      xvfb xdotool openbox \
      x11-utils scrot bash \
      curl ca-certificates xz-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# --- Browser engine (baked prebuilt, selected by CUTTLE_BROWSER_BINARY). ---
# Extracted in the download stages above. clark is the default; clearcote the
# fallback (currently not baked). No proprietary binary.
COPY --from=clark /opt/clark /opt/clark
# COPY --from=clearcote /opt/clearcote /opt/clearcote

# --- VNC viewer stack (KasmVNC, runtime-gated by CUTTLE_VNC) ---
# For the daily-driver login-handoff flow: a human opens KasmVNC in a browser
# tab to view/interact with the live stealth Chromium (sign in, solve captchas)
# on the SAME session the agent drives over CDP. KasmVNC's Xvnc BECOMES the :99
# display (headed, full X server) and serves the web client + websocket itself
# in one process (built-in noVNC, seamless clipboard). Installed always (~8MB)
# but only runs when CUTTLE_VNC=1 - the entrypoint then starts Xvnc instead of
# Xvfb, so the default (prod) image is unaffected. No apt repo upstream; the
# GitHub .deb is pinned. Base is trixie, so the trixie build matches.
# Pinned to a proven combo: KasmVNC 1.3.3 (bookworm
# .deb, installs cleanly on trixie) + stock noVNC 1.5.x client. Do NOT bump
# either independently: KasmVNC speaks a forked RFB dialect (non-standard
# PointerEvent wire format, own message types) and newer stock noVNC (1.7)
# sends startup messages 1.3.3/1.4.0 close the connection on ("unknown
# message type" -> viewer drops the moment the mouse touches the canvas).
ARG KASMVNC_VERSION=1.3.3
RUN ARCH="$(dpkg --print-architecture)" \
    && curl -fsSL "https://github.com/kasmtech/KasmVNC/releases/download/v${KASMVNC_VERSION}/kasmvncserver_bookworm_${KASMVNC_VERSION}_${ARCH}.deb" -o /tmp/kasmvnc.deb \
    && apt-get update \
    && apt-get install -y --no-install-recommends /tmp/kasmvnc.deb \
    && rm -rf /tmp/kasmvnc.deb /var/lib/apt/lists/*

# Minimal browser-only viewer page (no KasmVNC toolbar/UI) served by Xvnc's
# -httpd. Stock noVNC ES modules from the GitHub tag (core/ + vendor/ are
# browser-loadable as-is - no build step); the page autoconnects to
# /websockify on the same port. 1.5.0 = the version the cbm-cdp-shim viewer
# has proven against KasmVNC daily.
ARG NOVNC_VERSION=1.5.0
RUN mkdir -p /opt/cuttle-www \
    && curl -fsSL "https://github.com/novnc/noVNC/archive/refs/tags/v${NOVNC_VERSION}.tar.gz" \
       | tar xz -C /opt/cuttle-www --strip-components=1 "noVNC-${NOVNC_VERSION}/core" "noVNC-${NOVNC_VERSION}/vendor"
COPY ops/docker/bin/vnc-viewer.html /opt/cuttle-www/index.html

# --- Windows font pack + font-enumeration lockdown ---
# The renamed Windows-family fonts (built in the `fontpack` stage) are the ONLY
# fonts fontconfig may see, so font-enumeration reports a Windows font list. No
# Microsoft font software is included - these are free fonts with only their
# `name` table rewritten. cuttle serve passes --fingerprint-fonts-dir=/opt/winfonts
# for the fork binaries.
COPY --from=fontpack /opt/winfonts /opt/winfonts
# 50-block-linux-aliases.conf neutralises the ONE leak fonts alone can't: Chromium/
# Skia (SkFontConfigInterface_direct) hardcodes a metric-compatible equivalence
# table, so a probe for a Linux family (e.g. "Liberation Sans") is accepted against
# our renamed "Arial" - it compares the REQUESTED family to the MATCHED font, which
# no fontconfig alias can intercept. The conf instead rewrites those names to the
# glyphless `cuttle-null` sink so the browser finds no glyphs and falls through to
# the CSS generic, exactly as a real Windows Chrome (no such family) would.
COPY ops/docker/50-block-linux-aliases.conf /etc/fonts/conf.d/50-block-linux-aliases.conf
# Register ONLY /opt/winfonts, drop the system font dirs from fonts.conf, and
# remove 30-metric-aliases.conf (its Arial<->Liberation Sans / Calibri<->Carlito
# / Cambria<->Caladea aliases would otherwise resolve Linux names to our fonts).
RUN printf '<?xml version="1.0"?>\n<!DOCTYPE fontconfig SYSTEM "fonts.dtd">\n<fontconfig><dir>/opt/winfonts</dir></fontconfig>\n' > /etc/fonts/conf.d/00-winfonts.conf \
    && rm -f /etc/fonts/conf.d/30-metric-aliases.conf \
    && sed -i \
        -e '\#<dir>/usr/share/fonts</dir>#d' \
        -e '\#<dir>/usr/local/share/fonts</dir>#d' \
        -e '\#<dir prefix="xdg">fonts</dir>#d' \
        -e '\#<dir>~/.fonts</dir>#d' \
        /etc/fonts/fonts.conf \
    && fc-cache -f \
    && echo "winfonts families:" && fc-list : family | sort -u

# The static Go daemon. `cuttle serve` is the multiplexer: it strips inline proxy
# creds and answers the proxy 407 over CDP (Fetch.continueWithAuth) so the fork
# binaries can use an authenticated residential proxy; stamps a synthetic
# browserContextId on service_worker CDP targets so CDP clients do not crash on
# them; and replicates the fork's launch flag set (UA, canvas/rects noise,
# brand/platform-version, Windows fonts dir).
COPY --from=builder /out/cuttle /usr/local/bin/cuttle

# Headed-mode entrypoint (Xvfb + openbox), then the user command.
COPY ops/docker/bin/docker-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# 9222: CDP (agent). 6080: KasmVNC web viewer (only served when CUTTLE_VNC=1).
EXPOSE 9222 6080
ENV DISPLAY=:99
# Default engine = clark. With the clearcote stage re-enabled, point
# CUTTLE_BROWSER_BINARY at /opt/clearcote/chrome to fall back to clearcote.
ENV CUTTLE_BROWSER_BINARY=/opt/clark/chrome

ENTRYPOINT ["/entrypoint.sh"]
CMD ["cuttle", "serve", "--headless=false"]
