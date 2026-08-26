#!/bin/bash
# Headed-mode entrypoint: bring up an X server + window manager, then exec the
# user command (cuttle serve). Headed Chrome is required to clear escalated
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
# In VNC mode we also make the browser presentable for a human viewer (a headed
# viewer launch): a bare positional URL, which cuttle serve
# passes through to Chrome's argv, so headed Chrome maps a visible top-level
# window (a pure CDP-scraping launch is windowless); --start-maximized so
# openbox sizes it to the full display; --test-type to suppress the "unsupported
# flag: --no-sandbox" infobar; swiftshader GL (no GPU here); and a dark browser
# UI (sites see prefers-color-scheme: dark - a common value).
if [ "${CUTTLE_VNC:-0}" = "1" ]; then
  # Size the framebuffer to the window the session browser will actually open.
  # That window is sized to the seed's fake screen (fingerprint coherence beats
  # filling the display), so a fixed 1920x1080 framebuffer left the viewer showing
  # a small window on a large black field. `cuttle viewer-geometry` resolves the
  # same seed the daemon will launch with; it fails - and we keep the default -
  # whenever the size is not knowable ahead of the launch (pool mode, or a
  # non-durable profile whose fingerprint is random per launch).
  #
  # It is handed the daemon's own argv ("$@", the command we exec below) so it
  # resolves mode/data-dir/durability with the SAME flags-over-env precedence the
  # daemon uses. Reading only the environment made it disagree with any operator
  # who passed a flag - the helm chart passes --keep-profile and --data-dir.
  GEOMETRY="$(cuttle viewer-geometry "$@" 2>/dev/null)" || GEOMETRY=1920x1080
  # setsid: the X server must outlive the stop signal. tini runs with -g, so a
  # `docker stop` SIGTERMs the whole process group at once - and an X server that
  # dies first takes headed Chrome down with it ("XIO: fatal IO error 104"),
  # before the daemon can snapshot the session's cookies. Its own session keeps
  # it out of that signal; the container teardown still reaps it a moment later,
  # once the daemon has exited and tini follows.
  # NEVER raise KasmVNC's DLP_Log to `verbose` while debugging: it percent-encodes
  # and logs FULL clipboard payloads and every keystroke in both directions
  # (common/rfb/ServerCore.cxx, VNCSConnectionST.cxx). The human handoff exists to
  # let a person type a password here, and cuttle masks its own log lines precisely
  # so credentials do not land in the profile volume - one debugging flag would
  # write them there in cleartext. It is off because we never pass it; keep it that
  # way. DLP_ClipSendMax / DLP_ClipAcceptMax / DLP_ClipDelay are the useful knobs on
  # that channel (all default to unlimited).
  #
  # -DisableBasicAuth does the OPPOSITE of what the name suggests: rather than
  # opening the endpoints, it makes every /api/* request return a hardcoded 401
  # (measured on the shipped image; confirmed in the KasmVNC v1.3.3 source and by a
  # Kasm maintainer in KasmVNC#268). The static viewer at / is unaffected. So
  # /api/get_screenshot is unreachable here by design - do not "fix" that by
  # dropping the flag: with -interface 0.0.0.0 the flag is also what keeps the API
  # shut. Reaching it would need an owner-bit user (kasmvncpasswd -u <name> -w -o
  # <file>), which is a deliberate decision, not a cleanup.
  setsid Xvnc :99 -geometry "$GEOMETRY" -depth 24 \
    -websocketPort "${CUTTLE_VNC_PORT:-6080}" \
    -rfbport -1 \
    -httpd /opt/cuttle-www \
    -sslOnly 0 -SecurityTypes None -DisableBasicAuth \
    -AlwaysShared \
    -interface 0.0.0.0 &
  # `--` separates the Chrome passthrough from cuttle serve's own flags: serve
  # parses flags strictly and treats only what follows `--` as Chrome argv. The
  # first `--` is set's end-of-options; the second lands as a literal argument.
  set -- "$@" -- about:blank --start-maximized \
    --test-type --disable-infobars --use-angle=swiftshader --force-dark-mode
else
  setsid Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp &
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
# Its own session for the same reason as the X server above: the window manager
# dying mid-shutdown is not fatal to Chrome, but there is no reason to make the
# teardown any noisier than it has to be.
DISPLAY=:99 setsid openbox &

exec "$@"
