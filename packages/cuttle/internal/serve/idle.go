package serve

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	vncEnv     = "CUTTLE_VNC"
	vncPortEnv = "CUTTLE_VNC_PORT"
	// defaultVNCPort mirrors the entrypoint's own default for Xvnc's websocket
	// port (ops/docker/bin/docker-entrypoint.sh).
	defaultVNCPort = 6080

	// sessionIdlePoll is how often session mode's idle clock checks for a lease or
	// a viewer. Neither comes or goes with an event the daemon can hang a timer
	// off, so they are polled - but only while no CDP client is attached, and each
	// check is one map lookup plus one read of /proc/net/tcp. It bounds how late
	// the reap can be, and the shortest viewer visit that is guaranteed to be seen.
	sessionIdlePoll = 10 * time.Second

	// tcpEstablished is the state column's value for an established connection in
	// /proc/net/tcp (TCP_ESTABLISHED = 1, printed as two hex digits).
	tcpEstablished = "01"
)

// procNetTCP are the connection tables the viewer probe reads. Both, because
// KasmVNC listens on the wildcard and a client that reaches it over IPv6 lands
// only in the second one.
var procNetTCP = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// sessionIdleWaitLocked reports how much longer session mode's one browser must
// stay up, given whether a lease or a viewer is on it right now: 0 means reap it
// now. It is only ever consulted with the connection refcount at zero (the timer
// is armed on the last disconnect, or on a launch nothing attached to), so those
// two are all that is left to check.
//
// Finding either in use stops the clock outright, and it restarts from the first
// check that finds neither - not from the last one that found one, because
// whatever held the browser may have let go at any moment in between. So the
// browser is only ever closed after a whole timeout with nothing on it, which is
// what the flag promises; the poll only adds up to one interval of lag on top.
func (p *chromePool) sessionIdleWaitLocked(seedKey string, inUse bool) time.Duration {
	poll := p.idleCheckEvery()
	if inUse {
		delete(p.idleDeadlines, seedKey)
		return poll
	}
	deadline, counting := p.idleDeadlines[seedKey]
	if !counting {
		deadline = time.Now().Add(p.idleTimeout)
		p.idleDeadlines[seedKey] = deadline
	}
	left := time.Until(deadline)
	if left <= 0 {
		return 0
	}
	return min(left, poll)
}

// idleCheckEvery is how long an armed idle timer runs before it fires. Pool mode
// has nothing to look at before the deadline; session mode checks for a lease or
// a viewer every poll, so a viewer that comes and goes inside the window is seen.
func (p *chromePool) idleCheckEvery() time.Duration {
	if p.mode == modeSession {
		return min(p.idleTimeout, sessionIdlePoll)
	}
	return p.idleTimeout
}

// sessionInUse reports whether anything other than a CDP connection is still
// using the session browser: an agent holding the driving lease, or a human
// watching the viewer.
func (p *chromePool) sessionInUse(seedKey string) bool {
	if _, held := p.leases.status(seedKey); held {
		return true
	}
	return p.viewerAttached != nil && p.viewerAttached()
}

// vncViewerAttached reports whether a human has the viewer open, by looking for
// an established TCP connection to Xvnc's websocket port in this container's own
// connection table.
//
// The connection table is the signal because it is the only one there is:
// KasmVNC's /api/* (get_frame_stats, which would answer this directly) is
// hardcoded to 401 by the -DisableBasicAuth the entrypoint passes, and opening
// that API up to read one boolean would mean an owner-bit user and an
// authenticated HTTP client in the reap path. An established socket to the
// websocket port, on the other hand, IS a viewer: the viewer page holds one open
// for as long as it is watching, and it is the daemon's own network namespace,
// so nothing else's traffic can appear in it.
//
// Off entirely when the container was not started with a viewer (CUTTLE_VNC), and
// false wherever /proc is not the Linux one - a daemon with no viewer to keep
// alive has nothing to hold the browser up for.
func vncViewerAttached() bool {
	if !parseBoolEnv(os.Getenv(vncEnv)) {
		return false
	}
	port := defaultVNCPort
	if v := strings.TrimSpace(os.Getenv(vncPortEnv)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}
	for _, path := range procNetTCP {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if establishedOnPort(string(data), port) {
			return true
		}
	}
	return false
}

// establishedOnPort reports whether a /proc/net/tcp table has an established
// connection whose LOCAL port is port. Columns are fixed: local_address is the
// second field as hex "ADDR:PORT", st the fourth. The listening socket itself is
// not established, so it never matches.
func establishedOnPort(table string, port int) bool {
	for line := range strings.SplitSeq(table, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != tcpEstablished {
			continue
		}
		_, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(hexPort, 16, 32)
		if err == nil && int(n) == port {
			return true
		}
	}
	return false
}
