# cuttle: a stealth-Chromium CDP farm.
#
# A patched CDP multiplexer (bin/cuttleserve) spawns one stealth Chrome per
# fingerprint seed, routing per-seed identity (fingerprint, proxy, geoip) over
# CDP. The Chrome engine is a FREE, redistributable stealth-Chromium fork
# (clark, MIT, default; clearcote, BSD-3, fallback) baked in as a prebuilt
# binary and selected via CLOAKBROWSER_BINARY_PATH. No proprietary binary.
#
# Clean base: FROM python:3.12-slim with zero CloakBrowser binary in any layer.
# amd64-only: clark/clearcote ship linux-x64 prebuilts, so the published image
# is linux/amd64 (the Python multiplexer itself is arch-agnostic).
FROM python:3.12-slim

# Chromium system libs + headed-mode stack (Xvfb/openbox: headed Chrome is
# REQUIRED to clear escalated anti-bot challenges) + fontconfig + base fonts +
# metric-compatible font families for the Windows font pack + xz (clearcote
# ships .tar.xz). No nodejs (cuttle ships no JS wrapper).
RUN apt-get update && apt-get install -y --no-install-recommends \
      libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libcups2 \
      libdbus-1-3 libdrm2 libxkbcommon0 libatspi2.0-0 libxcomposite1 \
      libxdamage1 libxfixes3 libxrandr2 libgbm1 libpango-1.0-0 \
      libcairo2 libasound2 libx11-xcb1 libfontconfig1 libx11-6 \
      libxcb1 libxext6 libxshmfence1 libglib2.0-0 libgtk-3-0 \
      libpangocairo-1.0-0 libcairo-gobject2 libgdk-pixbuf-2.0-0 \
      libxss1 libxtst6 \
      fonts-liberation fonts-noto-color-emoji fonts-unifont \
      fonts-freefont-ttf fonts-ipafont-gothic fonts-wqy-zenhei \
      fonts-tlwg-loma-otf \
      fontconfig fonts-liberation2 fonts-crosextra-carlito fonts-crosextra-caladea \
      xvfb xdotool openbox \
      curl ca-certificates xz-utils \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Vendored MIT multiplexer package (argument-builders + geoip + config). Install
# it and its serve/geoip runtime deps from the lockfile (reproducible), plus
# fonttools for the font-pack build step below. No binary prebake: the fork
# binary is baked below and CLOAKBROWSER_BINARY_PATH bypasses any download path.
COPY --from=ghcr.io/astral-sh/uv:0.11.27 /uv /usr/local/bin/uv
COPY pyproject.toml uv.lock README.md LICENSE THIRD-PARTY.md ./
COPY cuttle/ cuttle/
RUN uv export --frozen --no-default-groups --group build --no-emit-project --no-hashes -o /tmp/req.txt \
    && uv pip install --system --no-cache -r /tmp/req.txt \
    && uv pip install --system --no-cache --no-deps . \
    && rm /tmp/req.txt

# --- clark-browser (MIT): ungoogled-chromium 148 + --fingerprint-* patch series,
# the same flag dialect cuttleserve emits. Primary engine. ---
ARG CLARK_TAG=chromium-v148.0.7778.96-stealth5
ARG CLARK_ASSET=clark-browser-linux-x64.tar.gz
ARG CLARK_SHA256=30cca952d11d94ca3424ac184b100c88ba686bfb87f2aaf4668ac5767562bd67
RUN mkdir -p /opt/clark \
    && curl -fsSL "https://github.com/clark-labs-inc/clark-browser/releases/download/${CLARK_TAG}/${CLARK_ASSET}" -o /tmp/clark.tgz \
    && echo "${CLARK_SHA256}  /tmp/clark.tgz" | sha256sum -c - \
    && tar -xzf /tmp/clark.tgz -C /opt/clark \
    && rm /tmp/clark.tgz \
    && CLARK_BIN="$(find /opt/clark -maxdepth 3 -type f -name chrome | head -1)" \
    && test -n "${CLARK_BIN}" \
    && if [ "${CLARK_BIN}" != /opt/clark/chrome ]; then ln -sf "${CLARK_BIN}" /opt/clark/chrome; fi \
    && chmod +x "${CLARK_BIN}" \
    && echo "clark chrome -> ${CLARK_BIN}"

# --- clearcote-browser (BSD-3): Chromium 149 + --fingerprint-* patch series.
# Fallback engine. NOTE: its timezone flag is --timezone (not
# --fingerprint-timezone); cuttleserve special-cases that. Ships .tar.xz. ---
ARG CLEARCOTE_TAG=v0.1.0-pre.18
ARG CLEARCOTE_ASSET=clearcote-149.0.7827.114-linux-x64.tar.xz
ARG CLEARCOTE_SHA256=fd96497e921b4fc9f384a5c1377896c8ee7e8a3a1991835c0256b010811e97aa
RUN mkdir -p /opt/clearcote \
    && curl -fsSL "https://github.com/clearcotelabs/clearcote-browser/releases/download/${CLEARCOTE_TAG}/${CLEARCOTE_ASSET}" -o /tmp/clearcote.txz \
    && echo "${CLEARCOTE_SHA256}  /tmp/clearcote.txz" | sha256sum -c - \
    && tar -xJf /tmp/clearcote.txz -C /opt/clearcote \
    && rm /tmp/clearcote.txz \
    && CLEARCOTE_BIN="$(find /opt/clearcote -maxdepth 3 -type f -name chrome | head -1)" \
    && test -n "${CLEARCOTE_BIN}" \
    && if [ "${CLEARCOTE_BIN}" != /opt/clearcote/chrome ]; then ln -sf "${CLEARCOTE_BIN}" /opt/clearcote/chrome; fi \
    && chmod +x "${CLEARCOTE_BIN}" \
    && echo "clearcote chrome -> ${CLEARCOTE_BIN}"

# --- Windows font pack ---
# Some anti-bot JS font-enumerates for Windows families; a Windows-claiming
# fingerprint must present real family NAMES. Provide them via metric-compatible
# free fonts (Liberation, Carlito, Caladea) renamed to the Windows names - all
# from Debian main, no proprietary download. cuttleserve passes
# --fingerprint-fonts-dir=/opt/winfonts for the fork binaries.
COPY fonts/rename-fonts.py /tmp/rename-fonts.py
RUN set -e; \
    mkdir -p /opt/winfonts; \
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
    rm -f /tmp/rename-fonts.py; \
    printf '<?xml version="1.0"?>\n<!DOCTYPE fontconfig SYSTEM "fonts.dtd">\n<fontconfig><dir>/opt/winfonts</dir></fontconfig>\n' > /etc/fonts/conf.d/00-winfonts.conf; \
    fc-cache -f; \
    echo "winfonts families:"; fc-scan --format='%{family[0]}\n' /opt/winfonts/*.ttf | sort -u

# The patched multiplexer: strips inline proxy creds and answers the proxy 407
# over CDP (Fetch.continueWithAuth) so the fork binaries can use an authenticated
# residential proxy; stamps a synthetic browserContextId on service_worker CDP
# targets so playwright-core does not crash on them; replicates the fork's launch
# flag set (UA, canvas/rects noise, brand/platform-version, Windows fonts dir).
COPY bin/cuttleserve /usr/local/bin/cuttleserve
RUN chmod +x /usr/local/bin/cuttleserve && python3 -m py_compile /usr/local/bin/cuttleserve

# Headed-mode entrypoint (Xvfb + openbox), then the user command.
COPY bin/docker-entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 9222
ENV DISPLAY=:99
# Default engine = clark. Swap to /opt/clearcote/chrome to fall back.
ENV CLOAKBROWSER_BINARY_PATH=/opt/clark/chrome

ENTRYPOINT ["/entrypoint.sh"]
CMD ["cuttleserve", "--headless=false"]
