#!/bin/sh
# Load the persona's XKB keymap into an X display (default :99).
# Chrome builds navigator.keyboard.getLayoutMap() from the server's XKB keymap.
# The default us/pc105 map matches real Chrome on both personas in 47 of 48
# entries. The odd one is IntlBackslash (keycode 94, <LSGT>): X maps it to "<",
# a real Mac reports the section sign and real Windows a backslash. Override that
# key alone, per persona (arm64 = macOS, amd64 = Windows). xkbcomp ships in
# x11-xkb-utils, which xvfb depends on. Its own script so the validation gates,
# which bring up their own Xvfb, load the same map as docker-entrypoint.sh.
# The X server must run with -noreset, or it reloads the default map as soon as
# this script, its only client, disconnects.
if [ "$(uname -m)" = "aarch64" ]; then
  LSGT_SYMS="section, plusminus"
else
  LSGT_SYMS="backslash, bar"
fi
xkbcomp -w 0 - "${1:-:99}" <<XKB
xkb_keymap {
  xkb_keycodes { include "evdev+aliases(qwerty)" };
  xkb_types    { include "complete" };
  xkb_compat   { include "complete" };
  xkb_symbols  {
    include "pc+us+inet(evdev)"
    override key <LSGT> { type= "TWO_LEVEL", [ $LSGT_SYMS ] };
  };
  xkb_geometry { include "pc(pc105)" };
};
XKB
