#!/bin/bash
# Headed-mode entrypoint: bring up an X server + window manager, then exec the
# user command (cuttleserve). Headed Chrome is required to clear escalated
# anti-bot challenges that headless cannot.

# Clean up any stale Xvfb lock left behind by a previous container instance.
# `/tmp` is not a tmpfs in this image, so on `docker restart` the previous
# container's `/tmp/.X99-lock` survives, and Xvfb refuses to start with an
# existing lock - leaving the container with no X server and every Chrome
# launch dying with "Missing X server or $DISPLAY".
rm -f /tmp/.X99-lock /tmp/.X11-unix/X99

# Provide the :99 display. CUTTLE_VNC=1 uses KasmVNC's Xvnc (a headed X server
# that ALSO serves the web viewer + websocket in one process) so a human can
# view/interact with the live browser in a tab; otherwise plain Xvfb. Both are
# headed (escalated anti-bot challenges need headed Chrome). Plain HTTP, no auth
# on Xvnc: the loopback port mapping (run with -p 127.0.0.1:PORT:PORT) is the
# security boundary. Downstream (openbox, xdotool, Chromium) only needs :99 up.
#
# In VNC mode we also make the browser presentable for a human viewer (args
# match CloakBrowser Manager's launch): a bare positional URL, which cuttleserve
# passes through to Chrome's argv, so headed Chrome maps a visible top-level
# window (a pure CDP-scraping launch is windowless); --start-maximized so
# openbox sizes it to the full display; --test-type to suppress the "unsupported
# flag: --no-sandbox" infobar; swiftshader GL (no GPU here); and a dark browser
# UI (sites see prefers-color-scheme: dark - a common value).
if [ "${CUTTLE_VNC:-0}" = "1" ]; then
  Xvnc :99 -geometry 1920x1080 -depth 24 \
    -websocketPort "${CUTTLE_VNC_PORT:-6080}" \
    -rfbport -1 \
    -httpd /opt/cuttle-www \
    -sslOnly 0 -SecurityTypes None -DisableBasicAuth \
    -AlwaysShared \
    -interface 0.0.0.0 &
  set -- "$@" about:blank --start-maximized \
    --test-type --disable-infobars --use-angle=swiftshader --force-dark-mode
else
  Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp &
fi

# Wait for the X server to actually accept connections before starting the WM.
# A blind `sleep 1` races under a CPU-starved start: openbox can come up before
# X is ready, fail to connect, and never retry - leaving --start-maximized a
# silent no-op for the container's whole life. Poll instead (xdotool is already
# installed and needs a live X server to answer). Bounded to ~10s.
for _ in $(seq 1 50); do
  DISPLAY=:99 xdotool getdisplaygeometry >/dev/null 2>&1 && break
  sleep 0.2
done

# Window manager so headed --start-maximized is honored (bare Xvfb has no WM;
# without one the flag is a silent no-op and the window stays un-maximized).
DISPLAY=:99 openbox &

exec "$@"
