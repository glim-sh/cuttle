---
type: Finding
title: With a forced WebRTC IP, WebRTC's own DNS and media must stay inside the proxy
description: Under --fingerprint-webrtc-ip, and for a proxied seed without it, WebRTC resolved hostname ICE servers and hostname candidates through the system resolver, a DNS leak outside the proxy; patch 0057 makes those lookups fail (forced mode, or --fingerprint-webrtc-no-local-dns, which cuttle passes for a proxied seed without a forced IP) so TURN dials the hostname through the proxy; in forced mode it also disables TURN over UDP and ICE-TCP, drops every UDP send, and leaves TURN over TCP or TLS as the only media path. A forced IP overrides any WebRTC handling policy, and cuttle's WebRTC decisions do not read the server's Chrome passthrough.
tags: [stealth, webrtc, proxy, dns, leak]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-10-27T16:30:00+00:00" }
sources:
  - id: patch
    resource: /packages/browser/patches/0057-webrtc-fabricated-candidates.patch
    title: Patch 0057 - WebRTC candidates at --fingerprint-webrtc-ip
  - id: pool
    resource: /packages/cuttle/internal/serve/pool.go
    title: packages/cuttle/internal/serve/pool.go - proxied seed without a forced WebRTC IP
  - id: i139
    resource: https://github.com/glim-sh/cuttle/pull/139
    title: "PR #139 review - server passthrough and WebRTC pins"
---

# With a forced WebRTC IP, WebRTC must stay inside the proxy

With `--fingerprint-webrtc-ip=<ip>` a page sees what Chrome shows without a
camera or microphone grant: one `<uuid>.local` host candidate and one srflx at
the forced IP, fabricated without a STUN request.[^patch] Fabricating the
candidates is not enough. Every path by which WebRTC itself touches the
network has to stay inside the browser's proxy, or the real network shows
through.

- **DNS.** WebRTC resolves hostnames on its own: a TURN server given by name,
  a hostname candidate a page supplies. Those lookups went to the system
  resolver, outside the proxy - a DNS leak a page can trigger. In forced mode
  they now fail, and TURN dials the hostname through the proxy, which
  resolves it at the exit.[^patch]
- **UDP.** Every UDPPort send is dropped, so no direct connectivity check
  reaches a peer, and TURN over UDP is disabled, since it would leave from the
  real interface. ICE-TCP stays off because real Chrome 154 gathers no TCP
  candidate.[^patch]
- **Media.** TURN over TCP or TLS is the one remaining path; Chrome dials it
  through its proxy, as `disable_non_proxied_udp` allows. With no such TURN
  server, media cannot connect.[^patch]

The cost is observable: a page's own STUN server receives no binding request
(real Chrome sends four to a silent one), yet a srflx appears. Without the
switch nothing changes.[^patch]

The DNS leak is not specific to forced mode. A proxied seed without a forced
IP (a per-connection proxy without geoip) runs with the `disable_non_proxied_udp` policy,
which keeps media on the proxy but still lets WebRTC resolve a page's TURN
hostname through the system resolver - 8 local lookups on the test page, as in
stock Chrome. `--fingerprint-webrtc-no-local-dns` makes those lookups fail the
same way without forcing an IP; cuttle passes it next to the policy flags for
exactly that case, and TURN over TCP still connects through the proxy. With the
switch 0 lookups reach the resolver, without it the 8 come back.[^patch][^pool]

## A forced IP overrides the handling policy

Patch 0057 applies the forced IP after the handling policy has configured the
port allocator and resets the policy's restrictions, so with
`--fingerprint-webrtc-ip` set the page sees the fabricated candidates whatever
`--webrtc-ip-handling-policy` or `--force-webrtc-ip-handling-policy`
says.[^patch] A policy flag next to a forced IP is redundant, not hiding the
candidates. It follows that a launch which derives an exit IP for WebRTC
overrides a policy an operator pinned, and that the policy only takes effect
when no IP is forced.

cuttle decides which WebRTC flags to add from the connection's own arguments
and the flags it builds itself; the server's Chrome passthrough (`cuttle serve
... -- <flags>`) is appended after them and wins in Chrome, but none of those
decisions reads it. A passthrough `--fingerprint-webrtc-ip=auto` therefore
reaches Chrome as the literal value, which the binary rejects, turning forced
mode off while cuttle, having derived an IP itself, adds neither the policy
nor `--fingerprint-webrtc-no-local-dns`. Tracked in #139.[^pool][^i139]

[^pool]: packages/cuttle/internal/serve/pool.go - proxied seed without a forced WebRTC IP
[^i139]: PR #139 review - server passthrough and WebRTC pins

[^patch]: Patch 0057 - WebRTC candidates at --fingerprint-webrtc-ip
