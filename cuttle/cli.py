"""The `cuttle` CLI: manage a local stealth-browser container and its viewer.

The default viewer is the offscreen VNC stack baked into the image (headed
Chrome on Xvnc, browser-only noVNC page) - the clean, working path. `cuttle
view` is an experimental alternative that streams the page over CDP screencast
instead; see cuttle.view.

Design: this is a thin host-side orchestrator. `up` shells out to `docker run`
with the VNC env/ports; `login` and `status` talk to the container's CDP over
HTTP/WebSocket. It holds no long-lived state - the container is the state.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
import webbrowser
from typing import NoReturn

DEFAULT_NAME = "cuttle"
DEFAULT_IMAGE = "cuttle:local"
DEFAULT_CDP_PORT = 9222
DEFAULT_VNC_PORT = 6080


def _docker() -> str:
    exe = shutil.which("docker")
    if not exe:
        _die("docker not found on PATH - install Docker (or OrbStack) first.")
    return exe


def _die(msg: str, code: int = 1) -> NoReturn:
    print(f"cuttle: {msg}", file=sys.stderr)
    raise SystemExit(code)


def _run(args: list[str], *, capture: bool = False) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, text=True, capture_output=capture, check=False)


def _container_state(name: str) -> str | None:
    """Return 'running', 'exited', ... or None if the container doesn't exist."""
    r = _run(
        [_docker(), "inspect", "-f", "{{.State.Status}}", name],
        capture=True,
    )
    if r.returncode != 0:
        return None
    return r.stdout.strip() or None


def _cdp_ready(port: int, timeout: float = 0.5) -> dict | None:
    try:
        with urllib.request.urlopen(
            f"http://127.0.0.1:{port}/json/version", timeout=timeout
        ) as resp:
            return json.loads(resp.read())
    except (urllib.error.URLError, TimeoutError, OSError, json.JSONDecodeError):
        return None


def _wait_cdp(port: int, timeout: float = 30.0) -> dict | None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        v = _cdp_ready(port)
        if v:
            return v
        time.sleep(0.5)
    return None


def _urls(cdp_port: int, vnc_port: int) -> tuple[str, str]:
    return (f"http://127.0.0.1:{cdp_port}", f"http://127.0.0.1:{vnc_port}/")


def cmd_up(args: argparse.Namespace) -> int:
    """Start the container with VNC viewing on. Idempotent, and profile-
    preserving: a stopped container is restarted (its browser profile lives in
    the container's filesystem, so logins persist across down/up) rather than
    recreated. `--recreate` forces a clean container + fresh profile."""
    state = _container_state(args.name)
    if args.recreate and state is not None:
        _run([_docker(), "rm", "-f", args.name], capture=True)
        state = None

    # The profile policy becomes a cuttleserve arg only on the `docker run` path,
    # so an existing container keeps whatever it was created with.
    if state is not None and args.keep_profile is not None:
        print(
            f"cuttle: --keep-profile is fixed when the container is created; "
            f"'{args.name}' keeps its original setting (use --recreate to change it)",
            file=sys.stderr,
        )

    if state == "running":
        if not _cdp_ready(args.cdp_port):
            _die(
                f"container '{args.name}' is running but CDP on :{args.cdp_port} is not "
                f"answering - check `docker logs {args.name}` or `cuttle down` and retry."
            )
        _print_ready(args, "already running")
        return 0

    if state is not None:
        # Exists but stopped - restart it, keeping the profile (logins).
        r = _run([_docker(), "start", args.name], capture=True)
        if r.returncode != 0:
            _die(f"docker start failed:\n{r.stderr.strip()}")
        if not _wait_cdp(args.cdp_port):
            _die(f"container restarted but CDP on :{args.cdp_port} never came up.")
        _print_ready(args, "restarted")
        return 0

    docker_args = [
        _docker(),
        "run",
        "-d",
        "--name",
        args.name,
        "-p",
        f"127.0.0.1:{args.cdp_port}:9222",
        "--shm-size=2g",
    ]
    if not args.no_vnc:
        docker_args += ["-p", f"127.0.0.1:{args.vnc_port}:6080", "-e", "CUTTLE_VNC=1"]
    # cuttleserve defaults to port 9222 and auto-binds 0.0.0.0 in a container,
    # so pass neither - it only accepts the `=` form and has no --host flag;
    # the space forms would leak onto Chrome's argv as junk positional tabs.
    docker_args += [args.image, "cuttleserve"]
    if args.keep_profile is not False:
        docker_args.append("--keep-profile")

    r = _run(docker_args, capture=True)
    if r.returncode != 0:
        _die(f"docker run failed:\n{r.stderr.strip()}")

    if not _wait_cdp(args.cdp_port):
        _die(
            f"container started but CDP on :{args.cdp_port} never came up - "
            f"check `docker logs {args.name}`."
        )
    _print_ready(args, "ready", show_image=True)
    return 0


def _print_ready(args: argparse.Namespace, verb: str, *, show_image: bool = False) -> None:
    cdp, viewer = _urls(args.cdp_port, args.vnc_port)
    tail = f", image {args.image}" if show_image else ""
    print(f"cuttle {verb}  (container '{args.name}'{tail})")
    print(f"  CDP     {cdp}    # agent-browser --cdp {args.cdp_port}")
    if not args.no_vnc:
        print(f"  viewer  {viewer}")


def cmd_status(args: argparse.Namespace) -> int:
    state = _container_state(args.name)
    if state is None:
        print(f"cuttle: no container '{args.name}' (run `cuttle up`)")
        return 1
    version = _cdp_ready(args.cdp_port)
    cdp, viewer = _urls(args.cdp_port, args.vnc_port)
    print(f"container '{args.name}': {state}")
    if version:
        print(f"  CDP     {cdp}  ({version.get('Browser', 'up')})")
    else:
        print(f"  CDP     {cdp}  (not answering)")
    if not args.no_vnc:
        print(f"  viewer  {viewer}")
    return 0 if state == "running" and version else 1


def cmd_down(args: argparse.Namespace) -> int:
    """Stop the container gracefully (SIGTERM), which lets cuttleserve shut
    Chrome down cleanly - so it records a clean exit and never crash-restores
    junk tabs. The container (and its logged-in profile) is kept for the next
    `cuttle up`; pass --purge to remove it and discard the profile."""
    state = _container_state(args.name)
    if state is None:
        print(f"cuttle: no container '{args.name}'")
        return 0
    if state == "running":
        # -t 15 > cuttleserve's 5s Chrome-drain, so the clean exit completes.
        r = _run([_docker(), "stop", "-t", "15", args.name], capture=True)
        if r.returncode != 0:
            _die(f"docker stop failed:\n{r.stderr.strip()}")
    if args.purge:
        r = _run([_docker(), "rm", "-f", args.name], capture=True)
        if r.returncode != 0:
            _die(f"docker rm failed:\n{r.stderr.strip()}")
        print(f"cuttle: removed container '{args.name}' (profile discarded)")
    else:
        print(f"cuttle: stopped container '{args.name}' (profile kept; `cuttle up` to resume)")
    return 0


def cmd_login(args: argparse.Namespace) -> int:
    """Navigate the browser to a URL and hand back the viewer link."""
    if not _cdp_ready(args.cdp_port):
        _die(f"CDP on :{args.cdp_port} not answering - run `cuttle up` first.")
    from cuttle.cdp import navigate

    vnc_port = None if args.no_vnc else args.vnc_port
    try:
        title = asyncio.run(navigate(args.cdp_port, args.url, vnc_port))
    except Exception as exc:  # noqa: BLE001 - surface any CDP failure to the user
        _die(f"navigation failed: {exc}")
    _, viewer = _urls(args.cdp_port, args.vnc_port)
    print(f"navigated to {args.url}" + (f"  ({title})" if title else ""))
    if not args.no_vnc:
        print(f"open the viewer to sign in:  {viewer}")
        if not args.no_open:
            webbrowser.open(viewer)
    return 0


def cmd_view(args: argparse.Namespace) -> int:
    """EXPERIMENTAL: serve a CDP-screencast viewer instead of VNC."""
    if not _cdp_ready(args.cdp_port):
        _die(f"CDP on :{args.cdp_port} not answering - run `cuttle up` first.")
    from cuttle.view import serve

    print(f"cuttle view (experimental): CDP :{args.cdp_port} -> http://{args.listen}/")
    if not args.no_open:
        webbrowser.open(f"http://{args.listen}/")
    try:
        asyncio.run(serve(args.cdp_port, args.listen, target_url=args.url))
    except KeyboardInterrupt:
        pass
    return 0


def _add_common(p: argparse.ArgumentParser) -> None:
    p.add_argument("--name", default=DEFAULT_NAME, help=f"container name (default {DEFAULT_NAME})")
    p.add_argument("--cdp-port", type=int, default=DEFAULT_CDP_PORT, help="host CDP port")
    p.add_argument("--vnc-port", type=int, default=DEFAULT_VNC_PORT, help="host VNC viewer port")
    p.add_argument("--no-vnc", action="store_true", help="run without the VNC viewer")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="cuttle",
        description="Manage a local stealth-browser container and its viewer.",
    )
    sub = parser.add_subparsers(dest="cmd", required=True)

    up = sub.add_parser("up", help="start the container (idempotent) with VNC viewing")
    _add_common(up)
    up.add_argument("--image", default=DEFAULT_IMAGE, help=f"image (default {DEFAULT_IMAGE})")
    # default=None distinguishes "not passed" from an explicit choice, so the
    # restart path can warn that the flag is fixed at container creation.
    up.add_argument(
        "--keep-profile",
        action=argparse.BooleanOptionalAction,
        default=None,
        help="persist the browser profile across restarts (default on)",
    )
    up.add_argument(
        "--recreate",
        action="store_true",
        help="destroy any existing container and start fresh (discards the profile)",
    )
    up.set_defaults(func=cmd_up)

    login = sub.add_parser("login", help="navigate to a URL and open the viewer to sign in")
    _add_common(login)
    login.add_argument("url", help="URL to open in the browser")
    login.add_argument("--no-open", action="store_true", help="print the viewer URL, don't open it")
    login.set_defaults(func=cmd_login)

    st = sub.add_parser("status", help="show container + CDP state")
    _add_common(st)
    st.set_defaults(func=cmd_status)

    down = sub.add_parser("down", help="stop the container gracefully (keeps the profile)")
    _add_common(down)
    down.add_argument(
        "--purge", action="store_true", help="also remove the container and discard the profile"
    )
    down.set_defaults(func=cmd_down)

    view = sub.add_parser("view", help="EXPERIMENTAL: CDP-screencast viewer instead of VNC")
    _add_common(view)
    view.add_argument("--listen", default="127.0.0.1:6090", help="host:port for the viewer")
    view.add_argument("--url", default=None, help="navigate here before viewing")
    view.add_argument("--no-open", action="store_true", help="don't auto-open the viewer")
    view.set_defaults(func=cmd_view)

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
