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
import concurrent.futures
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
import webbrowser
from importlib import metadata, resources
from typing import NamedTuple, NoReturn

DEFAULT_NAME = "cuttle"
IMAGE_REPO = "ghcr.io/glim-sh/cuttle"
DEFAULT_CDP_PORT = 9222
DEFAULT_VNC_PORT = 6080


class _Driver(NamedTuple):
    """A CDP driver CLI cuttle knows how to route agents to.

    cuttle never bakes in driver documentation - each driver self-documents at
    runtime (`docs`), so instructions always match the installed version. The
    briefing only carries the attach incantation and where the docs live.
    """

    name: str
    attach: str  # .format(cdp=<http endpoint>, port=<cdp port>)
    docs: str
    install: str
    # None = never probe: browser-use treats unknown argv as harness input and
    # would launch its daemon from a mere version check.
    version_args: tuple[str, ...] | None


# Briefing order IS the fallback order: first installed entry is the default.
_DRIVERS = (
    _Driver(
        name="agent-browser",
        # Per-command --cdp, never `connect`: on macOS `connect` can relaunch its
        # own local Chrome instead of attaching ("[agent-browser] relaunched
        # browser") and then silently drives a logged-out browser that is not cuttle.
        attach="agent-browser --cdp {port} <cmd>   # --cdp on EVERY command; never `connect`",
        docs="agent-browser skills get core --full",
        install="npm install -g agent-browser",
        version_args=("--version",),
    ),
    _Driver(
        name="browser-use",
        attach="BU_CDP_URL={cdp} browser-use <<'PY' ... PY",
        docs="browser-use skill show",
        install="uv tool install browser-use",
        version_args=None,
    ),
    _Driver(
        name="playwright-cli",
        attach="playwright-cli attach --cdp={cdp}",
        docs="playwright-cli --help   # its 'Agent skill:' line -> full SKILL.md + references/",
        install="npm install -g @playwright/cli",
        version_args=("--version",),
    ),
)


def _cuttle_version() -> str:
    try:
        return metadata.version("cuttle-browser")
    except metadata.PackageNotFoundError:
        return "dev"


def _driver_version(name: str, exe: str, version_args: tuple[str, ...]) -> str | None:
    try:
        r = subprocess.run(
            [exe, *version_args],
            text=True,
            capture_output=True,
            timeout=5,
            env={**os.environ, "NO_UPDATE_NOTIFIER": "1"},
        )
    except (OSError, subprocess.SubprocessError):
        return None
    first = ((r.stdout or r.stderr).strip().splitlines() or [""])[0]
    if r.returncode != 0 or not first:
        return None
    # Some drivers echo their own name ("agent-browser 0.31.1"); the briefing
    # already prints the name, so keep just the version.
    return first.removeprefix(name).strip()[:40] or None


def _detect_drivers() -> list[tuple[_Driver, str | None]]:
    """Return (driver, version|None) for each installed driver, briefing order.

    Version probes run in parallel so the briefing stays fast; a probe failure
    degrades to a versionless line, never an error.
    """
    installed = [(d, shutil.which(d.name)) for d in _DRIVERS]
    installed = [(d, exe) for d, exe in installed if exe]
    if not installed:
        return []
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(installed)) as pool:
        futures = {
            d.name: pool.submit(_driver_version, d.name, exe, d.version_args)
            for d, exe in installed
            if d.version_args is not None
        }
        return [(d, futures[d.name].result() if d.name in futures else None) for d, _ in installed]


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


def _default_image() -> str:
    """The published image tag matching this CLI's version, so it never drives a
    cuttleserve it wasn't shipped with. An uninstalled checkout reports "dev"
    (no such tag), so fall back to `latest`."""
    v = _cuttle_version()
    return f"{IMAGE_REPO}:{v if v != 'dev' else 'latest'}"


def _inspect(name: str, field: str) -> str | None:
    """A single `docker inspect -f {{field}}` value, or None if the container
    doesn't exist / the field is empty."""
    r = _run([_docker(), "inspect", "-f", f"{{{{{field}}}}}", name], capture=True)
    return r.stdout.strip() or None if r.returncode == 0 else None


def _container_state(name: str) -> str | None:
    """Return 'running', 'exited', ... or None if the container doesn't exist."""
    return _inspect(name, ".State.Status")


def _container_image(name: str) -> str | None:
    return _inspect(name, ".Config.Image")


def _container_ports(name: str) -> str:
    """The container's real host<-container port bindings, verbatim from docker
    (e.g. '9222/tcp -> 127.0.0.1:9222'). Empty if it has none."""
    r = _run([_docker(), "port", name], capture=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def _print_logs_tail(name: str, lines: int = 20) -> None:
    r = _run([_docker(), "logs", "--tail", str(lines), name], capture=True)
    out = (r.stdout + r.stderr).strip()
    if out:
        print(f"  last {lines} log lines:")
        for line in out.splitlines():
            print(f"    {line}")


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

    # A container that never ran cleanly is a failed-run zombie (state "created"
    # from a `docker run` that died at network setup, or "dead") - it has no live
    # host port binding to restart into. Remove it and fall through to a fresh
    # run. Only "exited" is safe to restart: that is a clean `cuttle down`.
    if state is not None and state not in ("running", "exited"):
        _run([_docker(), "rm", "-f", args.name], capture=True)
        state = None

    # The profile policy and the image are baked in on the `docker run` path, so
    # an existing container keeps whatever it was created with. Say so rather
    # than appearing to honour a flag we are about to ignore.
    if state is not None and args.keep_profile is not None:
        print(
            f"cuttle: --keep-profile is fixed when the container is created; "
            f"'{args.name}' keeps its original setting (use --recreate to change it)",
            file=sys.stderr,
        )
    if state is not None and args.image:
        print(
            f"cuttle: --image is fixed when the container is created; '{args.name}' keeps "
            f"the image it was created with (use --recreate to change it)",
            file=sys.stderr,
        )

    if state == "running":
        v = _cdp_ready(args.cdp_port)
        if not v:
            _die(
                f"container '{args.name}' is running but CDP on :{args.cdp_port} is not "
                f"answering - run `cuttle status` to triage, then `cuttle down` and retry."
            )
        _print_briefing(args, "already running", browser=v.get("Browser"))
        return 0

    if state is not None:
        # Exists but stopped - restart it, keeping the profile (logins).
        r = _run([_docker(), "start", args.name], capture=True)
        if r.returncode != 0:
            _die(f"docker start failed:\n{r.stderr.strip()}")
        v = _wait_cdp(args.cdp_port)
        if not v:
            _die(
                f"container restarted but CDP on :{args.cdp_port} never came up - "
                f"run `cuttle status` to triage (a port mismatch is the usual cause)."
            )
        _print_briefing(args, "restarted", browser=v.get("Browser"))
        return 0

    args.image = args.image or _default_image()

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
        # A run that fails at network setup (e.g. host port taken) still leaves a
        # half-created container behind - remove it so it is not mistaken for a
        # restartable container on the next `cuttle up`.
        _run([_docker(), "rm", "-f", args.name], capture=True)
        _die(f"docker run failed:\n{r.stderr.strip()}")

    v = _wait_cdp(args.cdp_port)
    if not v:
        _die(
            f"container started but CDP on :{args.cdp_port} never came up - "
            f"run `cuttle status` to triage."
        )
    _print_briefing(args, "ready", browser=v.get("Browser"), show_image=True)
    return 0


def _print_briefing(
    args: argparse.Namespace,
    verb: str,
    *,
    browser: str | None = None,
    show_image: bool = False,
) -> None:
    """The briefing: the single dynamic source of truth an agent needs to drive
    cuttle. Live state + installed drivers with attach lines and their own
    self-doc commands. cuttle carries no driver docs of its own - see _Driver."""
    version = _cuttle_version()
    cdp, viewer = _urls(args.cdp_port, args.vnc_port)
    tail = f", image {args.image}" if show_image else ""
    engine = f"  ({browser})" if browser else ""
    print(f"cuttle {verb}  (container '{args.name}'{tail})  cuttle-browser {version}")
    print(f"  CDP     {cdp}{engine}")
    if not args.no_vnc:
        print(f"  viewer  {viewer}")
    print()
    print("Attach to THIS browser over CDP. NEVER launch your own browser or create a")
    print("new profile/context: logins live in this one and persist across down/up.")
    print()
    drivers = _detect_drivers()
    if drivers:
        print("drivers (listed in priority order; the first is the default):")
        for d, ver in drivers:
            print(f"  {d.name}" + (f"  {ver}" if ver else ""))
            print(f"    attach  {d.attach.format(cdp=cdp, port=args.cdp_port)}")
            print(f"    docs    {d.docs}")
        for d in _DRIVERS:
            if not any(inst.name == d.name for inst, _ in drivers):
                print(f"  {d.name}  not installed   (install: {d.install})")
        print("routing: use the first driver listed above unless the user names another")
        print("  (bu / bu-cli / browseruse = browser-use). If the named driver is not")
        print("  installed, use the first listed instead and tell the user you fell back.")
        print("docs: fetch each driver's own instructions with the `docs` command above -")
        print("  they match the installed version; do not rely on memory or stale copies.")
    else:
        print("drivers: none installed. STOP and ask the user what to install -")
        print("  default: all three; minimal: just agent-browser (the default driver).")
        for d in _DRIVERS:
            print(f"    {d.install}")
        print("  (drivers attach to cuttle's browser - skip their own browser downloads)")
    if not args.no_vnc:
        print("login walls / captcha: `cuttle login <url>`, then hand the user the viewer")
        print("  link to sign in or solve it - the CDP session stays logged in.")
    print("full cuttle guide: `cuttle skill`  (skip if the cuttle skill is already loaded")
    print(f"  in your context and its version matches {version}; rerun it if not)")


def cmd_status(args: argparse.Namespace) -> int:
    """Single diagnostic surface: the briefing when healthy, and when not, the
    real image, real host port bindings, and a log tail - so triage never needs
    a raw docker command."""
    state = _container_state(args.name)
    if state is None:
        print(f"cuttle: no container '{args.name}' (run `cuttle up`)")
        return 1
    v = _cdp_ready(args.cdp_port)
    if state == "running" and v:
        # Healthy: the CDP/viewer URLs are the container's real bindings (CDP just
        # answered on them); add the real image so status surfaces drift either way.
        _print_briefing(args, "running", browser=v.get("Browser"))
        if image := _container_image(args.name):
            print(f"  image   {image}")
        return 0

    # Unhealthy: report what is actually there, not what was requested. The most
    # common cause is a CDP port that does not match the container's real binding.
    cdp, viewer = _urls(args.cdp_port, args.vnc_port)
    print(f"container '{args.name}': {state}")
    print(f"  CDP     {cdp}  (not answering)" if not v else f"  CDP     {cdp}")
    if not args.no_vnc:
        print(f"  viewer  {viewer}")
    if image := _container_image(args.name):
        print(f"  image   {image}")
    if ports := _container_ports(args.name):
        print("  actual port bindings (start `up` with THESE ports, do not --recreate):")
        for line in ports.splitlines():
            print(f"    {line}")
    _print_logs_tail(args.name)
    print("  fix: `cuttle down && cuttle up` (keeps the profile), or")
    print("    `cuttle up --recreate` to rebuild from scratch (discards the profile).")
    return 1


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


def cmd_skill(args: argparse.Namespace) -> int:
    """Print SKILL.md (the agent usage guide) to stdout, so `cuttle skill` hands
    an agent the how-to-drive-cuttle doc. SKILL.md is bundled as package data
    (see pyproject package-data), so this works from a plain `pip install`."""
    text = resources.files("cuttle").joinpath("SKILL.md").read_text(encoding="utf-8")
    print(text, end="")
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
    # Both default=None so the restart path can tell "not passed" from an explicit
    # choice and warn that the setting is fixed at container creation.
    up.add_argument(
        "--image",
        default=None,
        help=f"image (default {_default_image()}; use `--image cuttle:local` for a local build)",
    )
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

    sk = sub.add_parser("skill", help="print the SKILL.md agent usage guide to stdout")
    sk.set_defaults(func=cmd_skill)

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
