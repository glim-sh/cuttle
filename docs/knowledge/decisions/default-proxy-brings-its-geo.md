---
type: Decision
title: The default proxy brings its geo; a connection's own proxy opts in
description: A browser launched on the server default proxy (--proxy / CUTTLE_PROXY, a context's proxy) resolves timezone, locale and WebRTC address from the exit IP unasked, while a connection's own ?proxy= keeps geoip=true as an explicit opt-in.
tags: [serve, proxy, geoip, timezone, identity]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-10-26T18:25:00+00:00" }
sources:
  - id: pr
    resource: https://github.com/glim-sh/cuttle/pull/132
    title: "fix(serve): resolve geo for the server default proxy"
  - id: pool
    resource: /packages/cuttle/internal/serve/pool.go
    title: getOrLaunch geo switch
  - id: test
    resource: /packages/cuttle/internal/serve/pool_test.go
    title: TestDefaultProxyResolvesGeo
  - id: readme
    resource: /README.md
    title: README, contexts - "geoip follows it automatically"
  - id: helm
    resource: /ops/helm/cuttle/values.yaml
    title: Helm values, proxy - "geoip auto-follows it"
  - id: ops
    resource: /docs/OPERATING.md
    title: "OPERATING.md, Pool mode and Running on a server"
---

# Decision

When the proxy a browser launches with came from the server default - the
`--proxy` flag or `CUTTLE_PROXY`, which is how a context's `proxy` and the Helm
chart's `proxy` reach the daemon - cuttle resolves its geo exactly as if the
connection had passed `geoip=true`: timezone and locale from the exit IP unless
pinned, and the exit IP as the WebRTC address.[^pool][^test] A proxy a
connection supplies itself with `?proxy=` keeps geoip as the caller's
opt-in.[^ops]

# Why the default proxy cannot wait to be asked

Session mode, the default since 0.13.0, has no connection that could ask:
`cuttle up`, the CLI and the supervisor's relaunch all attach with no query, so
a browser on the default proxy launched with `--proxy-server` and no
`--fingerprint-timezone`, reporting the image's UTC from a residential exit -
the geo-vs-timezone tell cuttle already removed for direct egress. The README
and the Helm values promised the follow-the-proxy behavior from the Go rewrite
on; the code never had it until #132.[^readme][^helm][^pr]

# Why a connection's own proxy stays opt-in

A pool client that passes `?proxy=` per connection owns the whole identity and
may pin `timezone` and `locale` on purpose: behind a rotating multi-country
exit, geoip would freeze each seed's zone to whatever its first exit was and
drift from every later one. Turning geoip on for those connections would
override a choice the caller made; the default proxy has no caller to make
it.[^ops]

# Cost accepted

Every launch on the default proxy now pays an uncached exit-IP lookup through
the proxy (plus a GeoIP database lookup when timezone or locale is unpinned)
before Chrome starts. On a working proxy that is one round trip next to a cold
Chrome start; a proxy that blackholes the echo services costs the echo timeouts
per launch, which is accepted because such a proxy leaves the browser unusable
anyway.[^pool]

[^pr]: fix(serve): resolve geo for the server default proxy
[^pool]: getOrLaunch geo switch
[^test]: TestDefaultProxyResolvesGeo
[^readme]: README, contexts - "geoip follows it automatically"
[^helm]: Helm values, proxy - "geoip auto-follows it"
[^ops]: OPERATING.md, Pool mode and Running on a server
