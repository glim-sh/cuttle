---
type: Finding
title: Loopback-only routes must judge the peer, not the Host header
description: Through 0.17.1 the daemon's loopback-only routes (secrets, state, downloads, snapshot, auth, grab, lease) checked only the Host and Origin headers, which any client writes, so any peer able to reach the CDP port could read the secret store; they now require a loopback TCP peer, and because a docker -p mapping delivers the host's connection from the bridge gateway, the CLI reaches them over docker exec on local and ssh.
tags: [serve, security, secrets, loopback, docker, k8s]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-11-04T20:10:00+00:00" }
sources:
  - id: guard
    resource: /packages/cuttle/internal/serve/http.go
    title: rejectUntrustedLoopback and isLoopbackPeer
  - id: client
    resource: /packages/cuttle/internal/cli/secret.go
    title: daemonAPI, daemonFor and execTransport
  - id: probe
    resource: "probe on Docker 29.8 (OrbStack), 2026-11-04: a server in a container published with -p 127.0.0.1:PORT saw the host's request from the bridge gateway 192.168.215.1, an in-container request from 127.0.0.1; a sibling container sending Host: 127.0.0.1:9222 got 200 from GET /lease on the 0.17.1 image and 403 on the fixed build"
    title: Peer-address probe
  - id: containerd
    resource: https://github.com/containerd/containerd/blob/4ba2e4619ec009c94acf8cc7b94560363bea3f91/internal/cri/server/sandbox_portforward_linux.go
    title: containerd CRI portForward dials localhost inside the pod netns
  - id: crio
    resource: https://github.com/cri-o/cri-o/blob/a2740c1685044acff605f2e9f6a67ea48dce816c/internal/oci/runtime_oci_linux.go
    title: CRI-O PortForwardContainer dials localhost inside the pod netns
  - id: helm
    resource: /ops/helm/cuttle/templates/networkpolicy.yaml
    title: The chart's deny-all-ingress NetworkPolicy
---

# Finding

`rejectUntrustedLoopback` guards every route that hands out session data -
the secret store (`/secret`, capture), per-seed state, downloads, `/snapshot`,
`/auth`, `/grab`, the window raise and `/lease`. Through 0.17.1 it admitted a
request whenever the `Host` header named a loopback host and the `Origin`
header was absent or same-origin. Both are written by the client, and the
daemon binds `0.0.0.0:9222` inside a container, so any peer that could reach
the port - another container on the docker network, another pod - could send
`Host: 127.0.0.1` and use these routes: list, overwrite or delete held
secrets, read cookies through the state API, pull downloads. The CLI even did
it deliberately, rewriting `Host` to loopback when it dialled an IP literal.
The Host check is a DNS-rebinding defense for a browser on the same machine;
it never says where the connection came from.[^guard][^probe]

The guard now requires `r.RemoteAddr` to be a loopback address (`127.0.0.0/8`,
`::1`, IPv4-mapped forms included) before the Host and Origin checks, which
stay as the rebinding defense.[^guard]

# Who still reaches the routes

- **Inside the container** (`cuttle pw`'s snapshot fetch, the lease, any
  `docker exec ... curl`): peer `127.0.0.1`.
- **k8s**: `kubectl port-forward` ends in the container runtime, which dials
  `localhost:<port>` inside the pod's network namespace (containerd and CRI-O
  both), so the daemon sees a loopback peer.[^containerd][^crio]
- **local and ssh**: the published `-p 127.0.0.1:PORT` mapping, and an
  `ssh -L` to it, arrive from the docker bridge gateway, not loopback. The CLI
  therefore carries these requests over curl in the container, on the same
  exec `cuttle pw` uses; CDP and `/json/*` still go over the mapped port.[^probe][^client]
- **Refused now**: a `direct` context reached through a docker mapping, an
  `ssh -L` to one or another host, and pool-mode clients calling the state or
  downloads API over a published port. They must call from inside the
  container's network namespace.

# What limited the exposure

The CDP endpoint itself was, and is, unauthenticated: a peer that reaches the
port can drive the browser, which already reaches everything the session is
signed into. The bug added the daemon's own stores to that, with no CDP
client needed: the secret store's names, replacing or deleting its values,
capturing a page value back out, and the state API's raw cookies. The defaults kept the
port off the network: `cuttle up` and the documented `docker run` publish on
`127.0.0.1` only, and the helm chart ships a deny-all-ingress NetworkPolicy, so
the reachable peers were other containers on the same docker network, pods in
a cluster without NetworkPolicy enforcement, or a deployment that published
the port wider.[^helm]

[^guard]: rejectUntrustedLoopback and isLoopbackPeer
[^client]: daemonAPI, daemonFor and execTransport
[^probe]: Peer-address probe
[^containerd]: containerd CRI portForward dials localhost inside the pod netns
[^crio]: CRI-O PortForwardContainer dials localhost inside the pod netns
[^helm]: The chart's deny-all-ingress NetworkPolicy
