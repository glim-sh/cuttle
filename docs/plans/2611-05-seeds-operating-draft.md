# Draft: OPERATING.md for phase 2 of 2611-05 (seeds)

Working draft for the integration lane of
[2611-05](2611-05-shared-tabs-and-seeds.md), written 2026-11-05 from its sections 1, 5 and 7
before any phase 2 lane landed. INT merges the section below into `docs/OPERATING.md`
(after "Remote agents over MCP"), makes the edits listed under "Passages this changes",
checks every claim against the merged code, and deletes this file. Lines marked
`[INT: ...]` are open points to settle against the landed lanes.

## Passages this changes in OPERATING.md

- **"One browser per container"** (opening paragraph): session mode no longer refuses every
  `?fingerprint=` seed. It accepts `main` and any seed a principal binds, and refuses the
  rest with the same 400. Point to the new section.
- **The viewer's focus prediction** ("with two windows both claiming focus, it is disabled
  rather than guessing"; "Prediction covers session mode"): the focus stream now samples
  the seed whose browser owns the X-focused window (fix (a) below).
- **"Creation-fixed settings"**: in session mode `--idle-timeout` now reaps non-main seeds
  (default 15 min). `main` is never reaped, and the warning about ignoring the flag goes.
- **"Remote agents over MCP"**:
  - the `--principals-file` row and the principals paragraph: `seed=<name>`, not only
    `seed=main`;
  - the routes table: add `/fingerprint/<seed>/json...` and `/fingerprint/<seed>/devtools/*`
    beside `/json` and `/devtools/*`, trusted network peer only;
  - "Revoke a caller": add that a rebind revokes as well;
  - "Shared tabs": separation between agents is now a seed in the same instance, not only a
    separate instance.
- **"Picking ports"** (`--context`/`--name` come first): add `--seed`. `[INT: confirm
  where lane G parses it on pw]`
- **"Secrets the session types for you"** ("Every verb acts on ONE session"): with seeds,
  `--seed` picks the browser inside the instance.

## Several browsers in one instance

A session-mode instance can run more than one browser. Each one is a **seed**, with its
own Chrome, profile, cookies, fingerprint, session lease, secrets and downloads. The
browser every instance already had is the seed `main`. It is always on, relaunches
itself, and keeps its data where it always was, so an instance with no seeds configured
behaves exactly as before.

Everything else is the instance's, shared by every seed:

- the proxy, timezone, locale and screen; there are no per-seed settings;
- the one display and its viewer;
- the trust boundary. Every trusted network peer of the instance reaches every seed in
  it, and there is no per-seed authorization.

So keep callers that must not reach each other's browsers in separate instances, for
example one instance for the owner only and another shared with unattended agents.

**A principal line binds a seed.** A principal line gains an optional third field:

```
hq     sha256:<hex>                 # drives main
alice  sha256:<hex>  seed=main      # the same
bot    sha256:<hex>  seed=work      # drives its own browser, "work"
```

- **Names:** a lowercase letter, then up to 31 lowercase letters, digits or `_`
  (`^[a-z][a-z0-9_]{0,31}$`). `state` and `logs` are reserved, because they are
  directories of the data dir.
- **A bad line fails the load:** an invalid or reserved name fails it like any other
  bad line, and a failed load refuses every bearer until the file is valid again.
- **Only bound seeds exist.** In session mode the daemon accepts a seed only while some
  principal line binds it. Any other name is refused everywhere: on CDP, on the
  loopback routes and in the CLI. Principals are read only with `--remote-listen`, so
  seeds need remote mode too. Several principals may bind the same seed, and then share
  its tabs and its lease like principals on `main` do.
- **The fingerprint is not the name.** Each seed draws a random fingerprint the first
  time it starts and keeps it in `<data_dir>/<seed>.seed`, as `main` does. Names recur
  across instances, and a fingerprint must not.
- **A rebind is a full revoke.** Moving a principal to another seed, or deleting its
  line, ends its outstanding links, viewer sessions, file handles, lease and browser
  session with the reload. A call admitted under the old binding cannot mint a link
  or file handle after the reload.

**Start on first use, stop when idle.** A non-main seed's Chrome launches the first
time something needs it:

- an MCP call from a principal bound to it;
- a `cuttle --seed` verb;
- a CDP connect to its route;
- the redeem of a viewer link minted for it.

In session mode `--idle-timeout` applies to non-main seeds only, and defaults to 15
minutes. A seed is not reaped while it is busy, meaning while any of these holds:

- a CDP client is connected;
- its lease is live;
- a link minted for it has not expired;
- a viewer session for it is open;
- its Playwright MCP child still serves a session.

On reap the daemon captures the seed's cookies, stops Chrome and drops its secrets. The
next use starts it again with the same profile. So cookies survive a reap and a pod
restart, and secrets survive neither. A seed whose line is later removed keeps that
capture and re-inject for as long as its browser runs. `[INT: confirm against P1]`

**What each seed gets over MCP:**

- **A Playwright MCP child of its own.** It starts with the seed's first session and
  stops when the seed's last session ends. `main`'s child stays on `127.0.0.1:8931`.
  Every child runs with `--no-webmcp` and accepts only its own random Host name.
- **Its own downloads.** A child writes to `<data_dir>/<seed>/Downloads`, so a
  principal's `cuttle_download` finds only its seed's files.
- **Nothing crosses seeds.** Two seeds hold two leases and two secret stores, and no
  value of one seed is a mask pair in another's results.
- **Capabilities carry the seed.** Links, viewer sessions and file handles each carry
  the seed, so a link opens the seed it was minted for. A `cuttle_request_secret` form
  stores into that seed.

**One display, shared.** Every seed's window opens on the instance's one X display.
The viewer shows that display, whichever window is in front. Two fixes keep a handoff
usable:

- **(a) The phone keyboard follows X input.** The focus stream samples only the seed
  whose browser owns the window with X focus. The lookup is cached for at most a
  second, and falls back to `main`.
- **(b) Nothing steals focus.**
  - The CDP proxy adds `focus: false` to a driver's `Target.createTarget`, unless the
    driver set `focus` or `background` itself. A new tab in one seed therefore does not
    raise its window over the one the person is using.
  - Redeeming a viewer link starts the link's seed if needed and raises its window,
    and that seed becomes the front seed.
  - After any other non-main seed's Chrome launches, the daemon raises the front seed
    again, or `main` if the front seed is not running.

What stays shared is accepted. The person can see, click into and type into any seed's
window, and keyboard focus and the clipboard are one for all seeds. A clipboard capture
reads whatever the person last copied, for any seed. If that matters, use separate
instances.

**Raw CDP per seed.** Every seed has the root form's routes under a prefix:

```
GET /fingerprint/<seed>/json/version   /json/list   /json
GET /fingerprint/<seed>/devtools/...
```

- **Where they are served:** on the internal listener, and on the remote listener only
  for trusted network peers. They are the same handlers and follow the same rules as
  `/json` and `/devtools/` (see "One URL for your own devices").
- **The prefix carries through.** Discovery advertises the prefixed form in
  `webSocketDebuggerUrl` and in each tab's `devtoolsFrontendUrl`, so a client that
  follows discovery stays on its seed.
- **`main` keeps both forms.** `/fingerprint/main/` and the root form both name `main`,
  and count as the same browser for connection accounting.
- **Pages cannot use them.** An unbound name is refused. In remote mode the internal
  listener refuses every request a browser makes, so a page cannot start or drive a
  seed through discovery.

```bash
playwright-cli attach --cdp=https://<public-url>/fingerprint/work
```

**The CLI's `--seed`.** A context can name a seed (`seed = "work"` in its stanza), or a
verb can take `--seed`. It works on `pw`, `secret`, `downloads`, `auth`, `open` and
`viewer`.

- **`main`, or no seed, is unchanged byte for byte:** driver session `cuttle`, workdir
  `/data/__default__/Downloads`, the root CDP attach. An old CLI keeps working against
  `main`.
- **Any other seed** gets:
  - its own driver session `cuttle-<seed>`;
  - an attach to `http://127.0.0.1:9222/fingerprint/<seed>`;
  - the workdir `<data_dir>/<seed>/Downloads`;
  - `?fingerprint=<seed>` on every loopback call (the lease, the inline secret gate,
    `/snapshot`, `/mask`, `/window/raise`). Its lease, secrets, snapshots, masking and
    downloads are therefore its own.
- **Two contexts, one pod.** Two contexts can point at the same pod with different
  seeds, and drive two browsers side by side.

```bash
cuttle --context box --seed work pw snapshot     # [INT: the flag position lane G ships]
```

**Rollout and rollback.** Roll the image out first, then the principal lines, then the
CLI. Roll back in reverse.

- **Image before lines.** The phase 1 image accepts only `seed=main`, and any other
  `seed=` there refuses every bearer. Write non-main lines only once this image is live,
  and remove them before rolling the image back.
- **A removed seed's files stay on disk:** its dir `<data_dir>/<seed>/`, `<seed>.seed`
  and `state/<seed>.json`. They are never attached to `main` or to another seed. Delete
  them by hand. Binding the same name again picks them up. `[INT: confirm]`
- **A restart loses memory state:** links, viewer sessions, file handles and secrets,
  for every seed.
- **Old and new CLIs:** an old CLI reaches `main` unchanged. A new CLI with a non-main
  seed against an image without seeds is refused.
