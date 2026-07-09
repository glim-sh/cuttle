# cuttle smoke harness

A neutral, self-contained smoke that drives a running cuttle over CDP and
introspects each seed's browser directly - no third-party sites, no network
targets, no local server. Raw CDP over `websockets` (the dependency cuttle
already ships), so it stays pure-Python with no browser-automation toolchain.

## What it checks

1. **per-seed fingerprint isolation.** Each fingerprint seed gets its own
   coherent identity, so an in-page canvas readback differs across seeds.
2. **stealth coherence.** `navigator.webdriver` is falsy and the UA/platform
   agree (a Windows UA must not pair with a non-Windows platform).
3. **connection stability under cold-cycle load.** Fresh seeds are launched in a
   loop; every cycle must connect and probe without error.

Everything is read in-page on an `about:blank` target - nothing leaves the harness.

## Run

From the repo root (`websockets` is a declared dependency, so the project env has it):

```bash
uv sync
CUTTLE_URL=http://127.0.0.1:9222 uv run python test/harness.py && echo GREEN
```

## Config

| Env | Default | Meaning |
|-----|---------|---------|
| `CUTTLE_URL` | `http://127.0.0.1:9222` | cuttle CDP endpoint |
| `COLD_CYCLES` | `3` | Number of fresh-seed cold cycles |

## Green means

- Distinct canvas readbacks across seeds (per-seed fingerprint isolation works).
- Coherent stealth signals on every cycle (`navigator.webdriver` falsy,
  UA/platform agree).
- Every cold cycle connects and probes without error.

## Scope

This is a fast, client-agnostic local smoke of the mechanical stealth path. It
deliberately does NOT reproduce the playwright-core service_worker crash (only a
playwright client can observe that), nor does it clear real challenges. Those
are validated separately against a real amd64 deployment - see
[../docs/UPGRADE.md](../docs/UPGRADE.md).
