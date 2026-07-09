"""cuttle - authored host-side tooling.

This package is our own code (linted + typed), separate from the vendored
upstream subset under `vendor/cloakbrowser/`. It drives a running cuttle
container from the outside over Docker + CDP:

- `cuttle.cli`  - the `cuttle` command (up / login / view / status / down)
- `cuttle.view` - experimental CDP-screencast viewer (alternative to VNC)

Nothing here is imported by `bin/cuttleserve` or baked into the runtime path;
the container needs only `cloakbrowser`.
"""
