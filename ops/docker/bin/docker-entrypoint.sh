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

# Remote mode (a non-empty CUTTLE_REMOTE_LISTEN or --remote-listen, the flag
# winning as in the daemon) lets agents steer the browser, so a managed policy
# keeps its pages off the loopback services by address. A DNS name for
# 127.0.0.1 still reaches the CDP port and the MCP child, which refuse such a
# page themselves; the viewer has no port then (below).
REMOTE="${CUTTLE_REMOTE_LISTEN:-}"
prev=
for arg in "$@"; do
  case "$arg" in --remote-listen=*) REMOTE="${arg#*=}" ;; esac
  if [ "$prev" = --remote-listen ]; then REMOTE="$arg"; fi
  prev="$arg"
done
if [ -n "$REMOTE" ]; then
  mkdir -p /etc/chromium/policies/managed &&
    printf '{"URLBlocklist":["localhost","127.0.0.1","[::1]","0.0.0.0"]}\n' \
      >/etc/chromium/policies/managed/cuttle-remote.json ||
    echo "warning: could not write the loopback URL policy" >&2
else
  rm -f /etc/chromium/policies/managed/cuttle-remote.json
fi

# Provide the :99 display: Xvfb in both modes, so a container with the viewer
# on renders exactly like one without it (same X server, same GLX path, same
# canvas pixels). Headed either way (escalated anti-bot challenges need headed
# Chrome). CUTTLE_VNC=1 adds the Selkies takeover viewer on CUTTLE_VNC_PORT
# (started below, once the display and keymap are up) and sizes the display to
# the session window.
#
# In VNC mode we also make the browser presentable for a human viewer (a headed
# viewer launch): a bare positional URL, which cuttle serve
# passes through to Chrome's argv, so headed Chrome maps a visible top-level
# window (a pure CDP-scraping launch is windowless); --start-maximized so
# openbox sizes it to the full display; and a dark browser UI (sites see
# prefers-color-scheme: dark - a common value).
GEOMETRY=1920x1080
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
  #
  # A resolved geometry also switches openbox to the config that maximizes the
  # browser window. The window is exactly the session size then, and Chrome's X11
  # backend shrinks any window requested at the full display size by 1px (so a
  # window manager does not mistake it for legacy fullscreen) - innerWidth 1727
  # against availWidth 1728, where real maximized Chrome reads equal. Maximizing
  # is the WM's resize, not Chrome's request, so it lands on the exact size and
  # outranks a placement saved in the profile. Never on the 1920x1080 fallback:
  # there it would stretch the window past the seed's own screen.
  #
  # Selkies captures a framebuffer whose width is a multiple of 8 and otherwise
  # grows the X screen to the next one, which would stretch the maximized
  # window past the seed's screen (a 1710-wide MacBook Air screen holding a
  # 1712-wide window). So the display is padded up front, and openbox keeps
  # the padding as a right margin it never maximizes into: the window stays
  # exactly the session width and the viewer shows a strip of at most 7px.
  if VIEWER_GEOMETRY="$(cuttle viewer-geometry "$@" 2>/dev/null)"; then
    WIDTH=${VIEWER_GEOMETRY%x*}
    PAD=$(((8 - WIDTH % 8) % 8))
    GEOMETRY="$((WIDTH + PAD))x${VIEWER_GEOMETRY#*x}"
    sed "s#<right>0</right>#<right>$PAD</right>#" /etc/xdg/openbox/rc-maximized.xml >/tmp/openbox-rc.xml
    OPENBOX_ARGS=(--config-file /tmp/openbox-rc.xml)
  fi
  # `--` separates the Chrome passthrough from cuttle serve's own flags: serve
  # parses flags strictly and treats only what follows `--` as Chrome argv. The
  # first `--` is set's end-of-options; the second lands as a literal argument.
  set -- "$@" -- about:blank --start-maximized \
    --disable-infobars --force-dark-mode
fi
# setsid: the X server must outlive the stop signal. tini runs with -g, so a
# `docker stop` SIGTERMs the whole process group at once - and an X server that
# dies first takes headed Chrome down with it ("XIO: fatal IO error 104"),
# before the daemon can snapshot the session's cookies. Its own session keeps
# it out of that signal; the container teardown still reaps it a moment later,
# once the daemon has exited and tini follows.
setsid Xvfb :99 -noreset -screen 0 "${GEOMETRY}x24" -nolisten tcp &

# Wait for the X server to actually accept connections before starting the WM.
# A blind `sleep 1` races under a CPU-starved start: openbox can come up before
# X is ready, fail to connect, and never retry - leaving --start-maximized a
# silent no-op for the container's whole life. Poll instead (xdotool is already
# installed and needs a live X server to answer). Bounded to ~10s.
for _ in $(seq 1 50); do
  DISPLAY=:99 xdotool getdisplaygeometry >/dev/null 2>&1 && break
  sleep 0.2
done

# The persona's keyboard layout (see the script for why). It only sticks because
# the X server runs with -noreset: xkbcomp is the only client at this point, and
# an X server that resets when its last client leaves reloads the default map.
/usr/local/bin/xkb-persona-keymap.sh :99 || echo "warning: persona keymap not loaded" >&2

# Window manager so headed --start-maximized is honored (bare Xvfb has no WM;
# without one the flag is a silent no-op and the window stays un-maximized).
# Its own session for the same reason as the X server above: the window manager
# dying mid-shutdown is not fatal to Chrome, but there is no reason to make the
# teardown any noisier than it has to be.
DISPLAY=:99 setsid openbox "${OPENBOX_ARGS[@]}" &

# The Selkies takeover viewer: page, assets and one WebSocket on one plain-HTTP
# port, listening on every interface inside the container; the loopback port
# mapping (run with -p 127.0.0.1:PORT:PORT) is the security boundary, as it is
# for CDP. Why each flag: docs/knowledge/decisions/selkies-is-the-takeover-viewer.md.
# Selkies binds spare keycodes for characters the persona keymap lacks, which
# navigator.keyboard.getLayoutMap() shows: SELKIES_RUN_AFTER_DISCONNECT puts the
# persona keymap back once the last viewer leaves, and the loop does after a
# crash, which skips that hook. A viewer that keeps failing backs off to a
# minute between tries. The settings live in a subshell so they never reach
# the daemon or Chrome. Remote mode: the daemon's viewer proxy (its remote port's
# root) is the only way to the viewer, so it listens on a Unix socket in a private directory
# (the daemon's --viewer-upstream default), which no page in the browser can
# reach the way it reaches a loopback port.
if [ "${CUTTLE_VNC:-0}" = "1" ]; then
  VIEWER_LISTEN=(--public --port="${CUTTLE_VNC_PORT:-6080}")
  if [ -n "$REMOTE" ]; then
    mkdir -p -m 0700 /tmp/cuttle-viewer
    VIEWER_LISTEN=(--unix-socket=/tmp/cuttle-viewer/viewer.sock)
  fi
  (
    export SELKIES_RUN_AFTER_DISCONNECT="/usr/local/bin/xkb-persona-keymap.sh :99"
    export SELKIES_UI_TITLE=cuttle SELKIES_FILE_TRANSFERS=none
    export SELKIES_UI_SIDEBAR_SHOW_APPS=false SELKIES_UI_SIDEBAR_SHOW_FILES=false \
      SELKIES_UI_SIDEBAR_SHOW_GAMEPADS=false SELKIES_UI_SIDEBAR_SHOW_SHARING=false \
      SELKIES_UI_SIDEBAR_SHOW_WEBCAM=false SELKIES_UI_SIDEBAR_SHOW_AUDIO_SETTINGS=false \
      SELKIES_UI_SIDEBAR_SHOW_GAMING_MODE=false
    delay=2
    while :; do
      started=$SECONDS
      selkies "${VIEWER_LISTEN[@]}" \
        --enable-https=false --enable-basic-auth=false \
        --mode=websockets --enable-dual-mode=false \
        --enable-resize=false --second-screen=false --scaling-dpi=96 \
        --audio-enabled=false --microphone-enabled=false --webcam-enabled=false \
        --gamepad-enabled=false --printing-enabled=false --enable-sharing=false \
        --encoder=h264enc --rate-control-mode=crf --video-streaming-mode=false \
        --use-paint-over-quality=true
      echo "viewer: selkies exited ($?), restarting in ${delay}s" >&2
      /usr/local/bin/xkb-persona-keymap.sh :99 || echo "warning: persona keymap not restored" >&2
      sleep "$delay"
      if [ $((SECONDS - started)) -gt 60 ]; then delay=2; elif [ "$delay" -lt 60 ]; then delay=$((delay * 2)); fi
    done
  ) &
fi

exec "$@"
