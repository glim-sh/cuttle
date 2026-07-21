# CLI UX overhaul: idempotent backends, stable tunnels, leaner surface

Status: approved plan, 2026-07-21. Implementation phased; each phase lands as
reviewable commits on `feat/cli-ux-overhaul`.

## Motivation

Real-world use on the ssh backend (the backend the docs steer Apple Silicon
users toward) surfaced a cluster of lifecycle and reachability bugs, and an
agent-driven review of the CLI surface found discoverability gaps. Separately,
2026 CLI/AX guidance (clig.dev, clispec.dev, agent-experience writeups) sets a
bar the surface should meet: idempotent operations, errors as recovery
instructions, deterministic endpoints, honest help text, and a lean verb set.

cuttle's primary consumer is a coding agent attaching over CDP. Every fix below
is judged by that lens first.

## Verified bugs

All verified against source on 2026-07-21.

1. `SSH.Start` is not idempotent. It always issues a bare `docker run`
   (`internal/backend/ssh.go:60-73`) with none of `Local.Start`'s state
   machine (`internal/backend/local.go:65-109`): no running no-op, no
   `docker start` for an exited container, no zombie removal, and
   `StartOpts.Recreate` is silently ignored. Any existing container name is a
   hard failure; the "exited -> restart, keep profile" case cuttle is designed
   for crashes on ssh. `up`'s own help text says "idempotent".
2. `status` recommends `cuttle up --recreate` as a remedy on every backend
   (`internal/cli/commands.go:501-502`), but recreate only works on local
   (see 1) - the error hint prescribes a command that fails.
3. Briefing and hints label the context with the raw `--context` flag value,
   not the resolved name. `resolve()` computes and drops the active context
   name (`internal/cli/commands.go:155-173`); `locationLabel` receives
   `cf.contextName` (`commands.go:387`, `463`), so a context selected via
   `default_context` displays as `context ''`.
4. Ephemeral tunnel ports leak into the briefing on remote backends. `up` and
   `status` call `b.Reach(ctx, 0, 0)` (`commands.go:328`, `466`), which
   auto-picks free local ports (`internal/backend/backend.go:117-138`), prints
   them (e.g. `127.0.0.1:53371`), then `defer release()` tears the tunnel down
   on exit. The printed attach URLs are random per invocation and dead within
   ~60s (they only survive at all because of ssh `ControlPersist=60`,
   `ssh.go:111`). Agents faithfully relay these doomed URLs. Stable 9222/6080
   is only available via `connect`, which nothing advertises.
5. `cuttle login <url>` without `--profile` prints and opens a viewer URL,
   then returns immediately; the deferred release closes the ssh/k8s tunnel,
   killing the URL it just opened (`commands.go:536-594`). Only the
   `--profile` path holds the forward.
6. `serve --help` shows zero flags. `DisableFlagParsing: true`
   (`internal/serve/serve.go:96`) suppresses cobra's flag rendering; ~10 real
   flags (`--port`, `--idle-timeout`, `--fingerprint*`, ...) are invisible
   without reading `serve.go`.
7. Idle shutdown is unreachable from the CLI. The per-seed idle reaper exists
   (`internal/serve/pool.go:156-186`) but `runUp` never sets
   `StartOpts.IdleTimeout` (`commands.go:316-323`), no `up` flag exposes it,
   and the image sets no `CUTTLE_IDLE_TIMEOUT`. Default is 0 (off), so no
   CLI path enables reaping. Note: the plumbing below `up` already works for
   ssh - `SSH.Start` passes opts through the shared `dockerRunArgs`
   (`ssh.go:71` -> `local.go:148-150`); only `up` fails to set it. The k8s
   (helm) path is genuinely unplumbed.
8. Stale comments: `StartOpts.IdleTimeout` is annotated "local only"
   (`internal/backend/backend.go:72`) though `dockerRunArgs` is shared with
   ssh; `pool.go:156-157` claims "local runs pass a positive timeout" though
   no run ever passes one.
9. `context ls --context` flag help says "context to mark active"
   (`commands.go:721`); it marks nothing, it only changes which row gets the
   `*`.

## Design decisions (locked)

- D1: merge `login` + `connect` into one verb named `open`; `open [url]`
  optionally navigates and opens the viewer, always holds the session until
  Ctrl-C. `login` and `connect` remain as hidden aliases for one release;
  the `view` alias is removed.
- D2: every serve parameter is configurable by flag with `CUTTLE_*` env
  fallback (flag > env > default), via a small hand-rolled precedence helper.
  No viper/koanf dependency; cuttle already implements this precedence
  manually for context selection.
- D3: `--name` (multi-container-per-host) is removed. The multiplexer's
  per-seed isolation is the supported multi-identity mechanism; a fixed
  container name also deletes the `flagSuffix` remedy-echo machinery.
- D4: `context add` is added (flags-first, non-interactive-safe); it
  validates and writes config.toml. Contexts stop being hand-edit-only.
- D5: `--no-vnc` is removed (no concrete need; the viewer is the value prop).
  `CUTTLE_VNC` stays as the container-internal mechanism, always set.
- D6: remote backends get a persistent, deterministic tunnel owned by `up`
  (option A below), so CDP/viewer URLs are stable across all backends.
- D7: profile auth state becomes local-canonical BY DEFAULT (phase 3):
  the daemon supervises checkpoints, creds live on the operator's machine,
  and the remote container becomes ephemeral. `--keep-profile` is demoted to
  an escape hatch for sites whose sessions need full-profile fidelity
  (IndexedDB, service workers).

## Phase 1: lifecycle correctness

1. SSH state-machine parity. Extract the docker container lifecycle
   (inspect -> running no-op / exited `docker start` / zombie `rm` /
   `Recreate` -> `rm -f` + fresh run) from `Local.Start` into a shared
   helper parameterized over the runner argv prefix, used by both `Local`
   and `SSH`. Fixes bugs 1 and 2 (`--recreate` becomes real on ssh, so the
   `status` hint becomes true). `Local` behavior must not change; existing
   backend tests extend to cover the ssh paths.
2. Persistent pinned tunnel on remote backends (option A). `up` establishes
   the forward on the configured ports (default 9222/6080) and leaves it
   running: a detached ssh/`kubectl port-forward` process tracked by a
   pidfile under the XDG state dir (`$XDG_STATE_HOME/cuttle/`, default
   `~/.local/state/cuttle/`). `down` tears it down; `status` health-checks
   and re-establishes it. The briefing then prints the same stable
   `127.0.0.1:9222` / `:6080` on every backend. Fixes bugs 4 and 5
   structurally (ephemeral `Reach(0,0)` forwards remain only as internal
   fallback when no standing tunnel exists). The rejected alternative -
   keeping ephemeral tunnels and printing "run `cuttle open` to attach" -
   would mute the briefing on the most-used backend and cost every agent
   session an extra held terminal.
3. Resolved-context labels: `resolve()` returns the resolved context name;
   all labels/hints use it. Fixes bug 3.
4. `--idle-timeout` flag on `up`, plumbed through the existing
   `dockerRunArgs` path (works for local and ssh immediately); helm value
   for the k8s backend. Delete the two stale comments (bug 8). Fixes bug 7.

## Phase 2: leaner surface

Target end state: `up`, `down`, `status`, `open`, `context {ls,add}`,
`skill`, plus hidden `serve`.

1. `open [url]` (D1): merge of login/connect. Optional URL navigates and
   opens the viewer; `--profile` checks out/in as today; always holds until
   Ctrl-C. With the phase 1 standing tunnel, `open` no longer needs to pin
   ports itself. Hidden aliases `login`, `connect`; `view` removed.
2. Remove `--name` everywhere (D3); container name is fixed to `cuttle`.
   Delete `flagSuffix` and simplify every remedy hint.
3. Remove `--no-vnc` (D5).
4. `serve` gets real cobra flags (drop `DisableFlagParsing`), each with a
   `CUTTLE_*` env fallback via the precedence helper (D2); Chrome
   passthrough moves strictly behind `--` (cobra `ArgsLenAtDash`). Honest
   `--help`, and `Hidden: true` in the root command list (it is the image
   entrypoint, not a user verb). The existing env vars keep working
   unchanged; `-e CUTTLE_*` becomes the only channel `dockerRunArgs` uses.
5. `context add <name> --backend ssh|k8s|direct ... [--default]` (D4):
   validates input (and reachability where cheap), writes config.toml
   (create or update), `--default` sets `default_context`. Also fix the
   `context ls --context` flag description (bug 9).
6. Update the embedded `internal/cli/SKILL.md` and `context --help` to the
   new surface. SKILL.md changes are shipped behavior: commit as
   `feat(skill):` per docs/RELEASING.md.

Breaking notes for the release: `--name`, `--no-vnc`, and the `view` alias
are removed; `login`/`connect` become hidden aliases of `open`. Pre-1.0 this
is a minor bump via `feat!:`.

## Phase 3: local-canonical profiles by default

Goal (D7): creds never durably live on the remote box; the container is
disposable (`--recreate`, box loss, and `--purge` stop being credential-loss
events).

1. Daemon-side supervision: move the checkpoint loop into `serve`, which
   already owns each Chrome per seed. Extraction runs over the daemon's own
   loopback CDP using the same `internal/profile` extraction code (same
   module). Event-driven checkpoints: hook last-client-disconnect via the
   pool's existing per-seed refcounts (`pool.go:128-166`, the idle-reap
   mechanism) so state is captured the moment an agent detaches; keep a slow
   periodic timer as backstop for long-held connections.
2. Mux HTTP state API: `GET /profile/{seed}/state` and
   `PUT /profile/{seed}/state` using Go 1.22+ `http.ServeMux` patterns and
   `r.PathValue`, with `ETag`/`If-Match` on PUT so concurrent CLI processes
   cannot silently overwrite each other. stdlib only.
3. CLI wiring: `up` pushes local state for the default seed after CDP-ready;
   `down` pulls final state before stopping; `open --profile` keeps its
   session semantics but rides the same API. The default seed
   (`__default__`) becomes an implicit local-canonical profile.
4. Default flip: remote profile storage becomes ephemeral by default;
   `--keep-profile` remains as the explicit opt-in for full-profile-fidelity
   sites (IndexedDB / service-worker-bound sessions are not captured by
   storage_state). Document the fidelity limit where the flag is described.
5. Migration: a one-time state pull for existing containers (extract the
   running container's current cookies/localStorage into the local store)
   so flipping the default does not strand existing logins.
6. `log/slog` for daemon logs while touching `serve` internals (replaces the
   hand-rolled `logInfo`/`logWarn` prefixing).

## Explicitly rejected

- viper/koanf or any config framework (D2): the precedence helper is ~30
  lines; cuttle already does this by hand for context selection.
- Interactive wizard for `up`: zero-config local `up` already needs no
  decisions; interactive prompts are an AX anti-pattern. `context add`
  covers the real onboarding cliff.
- Full-profile-dir sync (tar/docker cp) as the default profile mechanism:
  full fidelity but heavyweight and checkpoint-less; revisit only if
  storage_state fidelity proves insufficient in practice.
- JSON output / differentiated exit codes / schema introspection: deferred,
  not rejected. Revisit after this overhaul; the briefing and `status` are
  the candidates (`clispec.dev` is the reference bar).

## References

- Command Line Interface Guidelines: https://clig.dev/
- The CLI Spec (agent-era conformance): https://clispec.dev/
- AX-first CLI design: https://frr.dev/posts/cli-agent-experience-design-llm-first
- Agent-ergonomic CLI principles: https://github.com/kunchenguid/axi
