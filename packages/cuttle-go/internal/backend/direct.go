package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/config"
)

// Direct targets a pre-exposed CDP/VNC endpoint from config as-is. It is the
// escape hatch for any browser cuttle does not itself manage (e.g. reached over
// a tailnet), so Start/Stop are errors.
type Direct struct {
	cdpHost string
	cdpPort int
	vncHost string
	vncPort int
	cdpURL  string
	// probe reports whether the CDP endpoint answers; injectable for tests.
	probe func(ctx context.Context, cdpURL string) bool
}

func newDirect(ctx config.Context) (*Direct, error) {
	if ctx.CDPURL == "" {
		return nil, errors.New("direct context requires cdp_url") //nolint:err113
	}
	cdpHost, cdpPort, err := splitHostPort(ctx.CDPURL)
	if err != nil {
		return nil, fmt.Errorf("invalid cdp_url %q: %w", ctx.CDPURL, err)
	}
	d := &Direct{cdpHost: cdpHost, cdpPort: cdpPort, cdpURL: ctx.CDPURL, probe: probeCDP}
	if ctx.VNCURL != "" {
		vncHost, vncPort, err := splitHostPort(ctx.VNCURL)
		if err != nil {
			return nil, fmt.Errorf("invalid vnc_url %q: %w", ctx.VNCURL, err)
		}
		d.vncHost, d.vncPort = vncHost, vncPort
	}
	return d, nil
}

func (d *Direct) State(ctx context.Context) (State, error) {
	if d.probe(ctx, d.cdpURL) {
		return StateRunning, nil
	}
	return StateAbsent, nil
}

func (d *Direct) Start(context.Context, StartOpts) error {
	return fmt.Errorf("direct context is a pre-exposed endpoint, %w - start the browser yourself", errNotManaged)
}

func (d *Direct) Stop(context.Context, bool) error {
	return fmt.Errorf("direct context is a pre-exposed endpoint, %w - stop the browser yourself", errNotManaged)
}

// Reach uses the configured URLs as-is; there is no tunnel to release.
func (d *Direct) Reach(context.Context, int, int) (Endpoint, func(), error) {
	return Endpoint{
		CDPHost: d.cdpHost, CDPPort: d.cdpPort,
		VNCHost: d.vncHost, VNCPort: d.vncPort,
	}, func() {}, nil
}

func splitHostPort(rawURL string) (string, int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", 0, err //nolint:wrapcheck
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, errors.New("missing host") //nolint:err113
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			return host, 443, nil
		}
		return host, 80, nil
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port %q", port) //nolint:err113
	}
	return host, p, nil
}

func probeCDP(ctx context.Context, cdpURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cdpURL+"/json/version", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode == http.StatusOK
}
