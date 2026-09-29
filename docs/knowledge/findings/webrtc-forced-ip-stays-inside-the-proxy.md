---
type: Finding
title: With a forced WebRTC IP, WebRTC's own DNS and media must stay inside the proxy
description: Under --fingerprint-webrtc-ip, WebRTC resolved hostname ICE servers and hostname candidates through the system resolver, a DNS leak outside the proxy; patch 0057 makes those lookups fail so TURN dials the hostname through the proxy, disables TURN over UDP and ICE-TCP, drops every UDP send, and leaves TURN over TCP or TLS as the only media path.
tags: [stealth, webrtc, proxy, dns, leak]
status: stable
generated: { by: claude-code/claude-opus-5-5, at: "2026-09-29T20:49:00+00:00" }
sources:
  - id: patch
    resource: /packages/browser/patches/0057-webrtc-fabricated-candidates.patch
    title: Patch 0057 - WebRTC candidates at --fingerprint-webrtc-ip
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

[^patch]: Patch 0057 - WebRTC candidates at --fingerprint-webrtc-ip
