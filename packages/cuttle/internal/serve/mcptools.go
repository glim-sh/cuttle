package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cuttle's own MCP tools. Each answers one text block whose JSON is mirrored in
// structuredContent (the SDK does both from the typed result), and the masking
// middleware covers them like every pass-through result.

const (
	viewerTTLDefault  = 15
	viewerTTLMax      = 30
	requestTTLDefault = 60
	requestTTLMin     = 15
	requestTTLMax     = 720
	waitDefault       = 10
	waitMax           = 15
	waitPoll          = time.Second
	waitProgressEvery = 15 * time.Second
	downloadInlineMax = 40 << 10
	reasonMax         = 300
)

var (
	errReasonRequired = errors.New("reason is required: one line the person will read on the link page")
	errBadSecretName  = errors.New("invalid secret name: letters, digits and underscore only, starting with a letter or underscore")
)

// autoArtifact matches the files Playwright MCP names itself (snapshots,
// screenshots, console dumps), which cuttle_download never serves: they hold
// page text unmasked, and "latest" means the newest download anyway.
var autoArtifact = regexp.MustCompile(`^[a-z]+-\d{4}-\d{2}-\d{2}T[0-9-]+Z\.[a-z]+$`)

func readOnlyTool() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
}

func closedWorld() *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{OpenWorldHint: &f}
}

func cleanReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", errReasonRequired
	}
	if utf8.RuneCountInString(reason) > reasonMax {
		reason = string([]rune(reason)[:reasonMax])
	}
	return reason, nil
}

func minutesIn(v, def, lo, hi int, field string) (time.Duration, error) {
	if v == 0 {
		v = def
	}
	if v < lo || v > hi {
		return 0, fmt.Errorf("%s must be between %d and %d", field, lo, hi) //nolint:err113 // argument feedback
	}
	return time.Duration(v) * time.Minute, nil
}

func (a *aggregator) addCuttleTools(s *agentSession) {
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "cuttle_status",
		Description: "What the browser holds: the domains it is signed in to (cookie counts and earliest expiry, never names or values), the secrets stored for this session (names, source, bound origin, never values), who holds the driving lease, and every shared open tab.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, statusOut, error) {
		return nil, a.status(ctx), nil
	})
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "cuttle_viewer_link",
		Description: "Mint a one-time link that opens the live browser for the person, for a login, captcha, 2FA or consent step only a human can do. Send the url to the user with one line of what to do, then call cuttle_wait. Minting the link does not change the selected tab.",
		Annotations: closedWorld(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in viewerLinkIn) (*mcp.CallToolResult, linkOut, error) {
		out, err := a.viewerLink(s, in)
		return nil, out, err
	})
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "cuttle_wait",
		Description: "Wait until a condition holds across all open tabs, polling browser metadata once a second: url:<glob> (a tab's url matches, * is any run), gone:<glob> (no tab's url matches), title:<substring>, or secret:<NAME> (the secret is stored and live). A timeout answers met:false, not an error. Reads the current page URL and title only at return and progress, every 15 s.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, waitOut, error) {
		out, err := a.wait(ctx, s, req, in)
		return nil, out, err
	})
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "cuttle_secret_capture",
		Description: "Store a value from your current tab as a secret without it entering this conversation: the value of the element a CSS selector names, or the clipboard. It is bound to the tab's origin, masked in every result from now on, and typed back as {{cuttle:NAME}}. All screenshots are refused for 30 s afterwards.",
		Annotations: &mcp.ToolAnnotations{},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in captureIn) (*mcp.CallToolResult, captureOut, error) {
		out, err := a.capture(ctx, s, in)
		return nil, out, err
	})
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "cuttle_request_secret",
		Description: "Ask the person for a credential through a one-time form link instead of the chat. The form shows who asks, the name, the origin it will be bound to (the person can correct it) and your reason; the value is never sent to you. Send the url to the user, then cuttle_wait with secret:<NAME>, then type {{cuttle:NAME}}.",
		Annotations: closedWorld(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in requestSecretIn) (*mcp.CallToolResult, linkOut, error) {
		out, err := a.requestSecret(s, in)
		return nil, out, err
	})
	mcp.AddTool(s.server, &mcp.Tool{
		Name:        "cuttle_download",
		Description: "Describe a file the browser downloaded and mint a url for it (valid 1 h, fetched with this server's bearer, e.g. curl -o). name is a bare filename or \"latest\". Text files under 40 KB are also inlined; binaries never are.",
		Annotations: readOnlyTool(),
	}, func(_ context.Context, _ *mcp.CallToolRequest, in downloadIn) (*mcp.CallToolResult, downloadOut, error) {
		out, err := a.download(s, in)
		return nil, out, err
	})
}

// ---------------------------------------------------------------------------
// cuttle_status
// ---------------------------------------------------------------------------

type leaseOut struct {
	Held       bool   `json:"held"`
	Owner      string `json:"owner,omitempty"`
	AgeSeconds int    `json:"age_seconds,omitempty"`
}

type tabOut struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

type statusOut struct {
	SignedIn []originAuth `json:"signed_in"`
	Secrets  []secretInfo `json:"secrets"`
	Lease    leaseOut     `json:"lease"`
	Tabs     []tabOut     `json:"tabs"`
}

func (a *aggregator) status(ctx context.Context) statusOut {
	seed := reservedSeed
	out := statusOut{SignedIn: []originAuth{}, Secrets: a.r.m.pool.secrets.list(seed), Tabs: []tabOut{}}
	if domains, ok := a.browser.cookies(ctx); ok {
		out.SignedIn = domains
	}
	if l, held := a.r.m.pool.leases.status(seed); held {
		out.Lease = leaseOut{Held: true, Owner: l.owner, AgeSeconds: int(time.Since(l.acquired).Seconds())}
	}
	if pages, err := a.browser.pages(ctx); err == nil {
		for _, p := range pages {
			out.Tabs = append(out.Tabs, tabOut{URL: p.URL, Title: p.Title})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// cuttle_viewer_link and cuttle_request_secret
// ---------------------------------------------------------------------------

type viewerLinkIn struct {
	Reason     string `json:"reason" jsonschema:"one line for the person: what they should do in the viewer"`
	TTLMinutes int    `json:"ttl_minutes,omitempty" jsonschema:"how long the link and the viewer session stay valid, 1 to 30 minutes (default 15)"`
}

type linkOut struct {
	URL       string `json:"url" jsonschema:"the one-time link to send to the person"`
	ExpiresAt string `json:"expires_at" jsonschema:"when the link stops working, RFC 3339"`
}

func (a *aggregator) viewerLink(s *agentSession, in viewerLinkIn) (linkOut, error) {
	reason, err := cleanReason(in.Reason)
	if err != nil {
		return linkOut{}, err
	}
	ttl, err := minutesIn(in.TTLMinutes, viewerTTLDefault, 1, viewerTTLMax, "ttl_minutes")
	if err != nil {
		return linkOut{}, err
	}
	expires := time.Now().Add(ttl)
	token, err := a.r.mintTicket(ticket{kind: ticketViewer, principal: s.principal, reason: reason, expires: expires})
	if err != nil {
		return linkOut{}, err
	}
	logInfo("remote: %s minted a viewer link (%s): %s", s.principal, ttl, reason)
	return linkOut{URL: a.r.link(token), ExpiresAt: expires.UTC().Format(time.RFC3339)}, nil
}

type requestSecretIn struct {
	Name       string `json:"name" jsonschema:"the secret's name, typed back later as {{cuttle:NAME}}; letters, digits and underscore"`
	Origin     string `json:"origin" jsonschema:"the site the value may be typed into, as an origin such as https://example.com; the person can correct it"`
	Reason     string `json:"reason" jsonschema:"one line for the person: why the value is needed"`
	TTLMinutes int    `json:"ttl_minutes,omitempty" jsonschema:"how long the form link stays valid, 15 to 720 minutes (default 60)"`
}

func (a *aggregator) requestSecret(s *agentSession, in requestSecretIn) (linkOut, error) {
	if !validSecretName(in.Name) {
		return linkOut{}, errBadSecretName
	}
	origin, err := secretOrigin(strings.TrimSpace(in.Origin))
	if err != nil || origin == "" {
		return linkOut{}, errSecretOrigin
	}
	reason, err := cleanReason(in.Reason)
	if err != nil {
		return linkOut{}, err
	}
	ttl, err := minutesIn(in.TTLMinutes, requestTTLDefault, requestTTLMin, requestTTLMax, "ttl_minutes")
	if err != nil {
		return linkOut{}, err
	}
	expires := time.Now().Add(ttl)
	token, err := a.r.mintTicket(ticket{
		kind: ticketSecret, principal: s.principal, reason: reason, name: in.Name, origin: origin, expires: expires,
	})
	if err != nil {
		return linkOut{}, err
	}
	logInfo("remote: %s requested secret %s for %s: %s", s.principal, in.Name, origin, reason)
	return linkOut{URL: a.r.link(token), ExpiresAt: expires.UTC().Format(time.RFC3339)}, nil
}

// ---------------------------------------------------------------------------
// cuttle_wait
// ---------------------------------------------------------------------------

type waitIn struct {
	Until          string `json:"until" jsonschema:"the condition: url:<glob>, gone:<glob>, title:<substring> or secret:<NAME>"`
	TimeoutMinutes int    `json:"timeout_minutes,omitempty" jsonschema:"how long to wait, 1 to 15 minutes (default 10)"`
}

type waitOut struct {
	Met       bool   `json:"met"`
	Condition string `json:"condition"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	WaitedS   int    `json:"waited_s"`
}

const (
	condURL    = "url"
	condGone   = "gone"
	condTitle  = "title"
	condSecret = "secret"
)

var errBadCondition = errors.New("until must be url:<glob>, gone:<glob>, title:<substring> or secret:<NAME>")

// compileGlob turns a url glob into a regexp: * is any run, everything else is
// literal - ? included, as in `cuttle navigate --wait` - since it is a query
// string's own delimiter.
func compileGlob(glob string) *regexp.Regexp {
	return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(glob), `\*`, ".*") + "$")
}

func (a *aggregator) wait(ctx context.Context, s *agentSession, req *mcp.CallToolRequest, in waitIn) (waitOut, error) {
	kind, arg, ok := strings.Cut(in.Until, ":")
	if !ok || arg == "" || !slices.Contains([]string{condURL, condGone, condTitle, condSecret}, kind) {
		return waitOut{}, errBadCondition
	}
	if kind == condSecret && !validSecretName(arg) {
		return waitOut{}, errBadSecretName
	}
	timeout, err := minutesIn(in.TimeoutMinutes, waitDefault, 1, waitMax, "timeout_minutes")
	if err != nil {
		return waitOut{}, err
	}
	var token any
	if req != nil && req.Params != nil {
		token = req.Params.GetProgressToken()
	}
	var glob *regexp.Regexp
	if kind == condURL || kind == condGone {
		glob = compileGlob(arg)
	}
	start := time.Now()
	lastProgress := start
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	for {
		out := a.evaluate(ctx, kind, arg, glob)
		out.Condition = in.Until
		out.WaitedS = int(time.Since(start).Seconds())
		if out.Met || time.Since(start) >= timeout {
			a.waitPage(ctx, s, &out)
			return out, nil
		}
		if token != nil && time.Since(lastProgress) >= waitProgressEvery {
			lastProgress = time.Now()
			a.waitPage(ctx, s, &out)
			_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
				ProgressToken: token, Progress: float64(out.WaitedS), Total: timeout.Seconds(),
				Message: maskExact("waiting for "+in.Until+"; now at "+out.URL, a.r.m.pool.secrets.heldPairs(reservedSeed)),
			})
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err() //nolint:wrapcheck // the agent cancelled
		case <-tick.C:
		}
	}
}

// evaluate polls browser metadata without running page scripts.
func (a *aggregator) evaluate(ctx context.Context, kind, arg string, glob *regexp.Regexp) waitOut {
	if kind == condSecret {
		for _, info := range a.r.m.pool.secrets.list(reservedSeed) {
			if info.Name == arg && info.Live {
				return waitOut{Met: true}
			}
		}
		return waitOut{}
	}
	pages, err := a.browser.pages(ctx)
	if err != nil {
		return waitOut{}
	}
	matched := false
	for _, p := range pages {
		switch kind {
		case condURL:
			if glob.MatchString(p.URL) {
				return waitOut{Met: true, URL: p.URL, Title: p.Title}
			}
		case condTitle:
			if strings.Contains(p.Title, arg) {
				return waitOut{Met: true, URL: p.URL, Title: p.Title}
			}
		case condGone:
			matched = matched || glob.MatchString(p.URL)
		}
	}
	return waitOut{Met: kind == condGone && !matched}
}

// ---------------------------------------------------------------------------
// cuttle_secret_capture
// ---------------------------------------------------------------------------

type captureIn struct {
	Name          string `json:"name" jsonschema:"the secret's name, typed back later as {{cuttle:NAME}}"`
	Selector      string `json:"selector,omitempty" jsonschema:"CSS selector of the element whose value to store, in your current tab"`
	FromClipboard bool   `json:"from_clipboard,omitempty" jsonschema:"store the clipboard's text instead of an element's value"`
}

type captureOut struct {
	Name        string `json:"name"`
	Sentinel    string `json:"sentinel" jsonschema:"what to type to use the value"`
	Length      int    `json:"length"`
	BoundOrigin string `json:"bound_origin" jsonschema:"the only origin the value will be typed into"`
}

var errCaptureSource = errors.New("give exactly one of selector or from_clipboard")

func (a *aggregator) capture(ctx context.Context, s *agentSession, in captureIn) (captureOut, error) {
	if !validSecretName(in.Name) {
		return captureOut{}, errBadSecretName
	}
	if (in.Selector == "") == !in.FromClipboard {
		return captureOut{}, errCaptureSource
	}
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if s.isRevoked() {
		return captureOut{}, errRevoked
	}
	if err := a.holdLease(s); err != nil {
		return captureOut{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, captureSelectorTimeout)
	defer cancel()
	if in.FromClipboard {
		if err := a.allowClipboard(ctx, s); err != nil {
			return captureOut{}, err
		}
	}
	selector, _ := json.Marshal(in.Selector)
	fn := "() => { const r = (" + captureSelectorJS + ")(" + string(selector) + "); return {...r, origin: location.origin}; }"
	if in.FromClipboard {
		fn = "async () => { const value = await navigator.clipboard.readText(); return {ok: !!value, value, origin: location.origin}; }"
	}
	raw, err := a.evalPage(ctx, s, fn)
	if err != nil {
		return captureOut{}, errCaptureFailed
	}
	var result struct {
		OK     bool   `json:"ok"`
		Value  string `json:"value"`
		Why    string `json:"why"`
		Origin string `json:"origin"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return captureOut{}, errCaptureFailed
	}
	if !result.OK || result.Value == "" {
		switch {
		case in.FromClipboard:
			return captureOut{}, fmt.Errorf("%w: the clipboard is empty", errCaptureFailed)
		case result.Why != "":
			return captureOut{}, fmt.Errorf("%w: %s", errCaptureFailed, result.Why)
		}
		return captureOut{}, errCaptureFailed
	}
	origin, err := secretOrigin(result.Origin)
	if err != nil || origin == "" {
		return captureOut{}, errSecretOrigin
	}
	value := []byte(result.Value)
	defer clear(value)

	// A capture may replace only an earlier capture for the same origin: never
	// a value the owner set or the person typed into a form.
	for _, e := range a.r.m.pool.secrets.list(reservedSeed) {
		if e.Name == in.Name && (e.Source != sourceCapture || e.BoundOrigin != origin) {
			return captureOut{}, fmt.Errorf("cuttle: a secret named %s is already stored (source %s); capture under another name", in.Name, e.Source) //nolint:err113 // one-off
		}
	}
	held := a.r.m.pool.secrets.putBound(reservedSeed, in.Name, slices.Clone(value), sourceCapture, 0, origin)
	a.captureMu.Lock()
	a.capturedAt = time.Now()
	a.captureMu.Unlock()

	logInfo("secrets: %s captured by %s (%d bytes, bound to %s, ttl %s)", in.Name, s.principal, len(value), origin, held)
	return captureOut{Name: in.Name, Sentinel: sentinelPrefix + in.Name + sentinelSuffix, Length: len(value), BoundOrigin: origin}, nil
}

// ---------------------------------------------------------------------------
// cuttle_download
// ---------------------------------------------------------------------------

type downloadIn struct {
	Name string `json:"name" jsonschema:"a bare filename from the download directory, or latest"`
}

type downloadOut struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime"`
	SHA256 string `json:"sha256"`
	URL    string `json:"url" jsonschema:"GET it with this server's bearer within 1 h"`
	Text   string `json:"text,omitempty" jsonschema:"the content, for a text file under 40 KB"`
}

var errDownloadBusy = errors.New("still downloading - retry")

// downloadSettle is how long a file must go unwritten before it is served.
const downloadSettle = time.Second

var errNoDownload = errors.New("no such download - the browser has not downloaded a file by that name")

// downloadDirs is where files a remote agent may fetch live: the driver's
// directory, where Playwright MCP saves downloads, then the browser's own.
func (a *aggregator) downloadDirs() []string {
	dirs := []string{driverDownloadsDir(a.r.m.pool.dataDir)}
	if inst := a.r.m.pool.runningInstance(reservedSeed); inst != nil && !slices.Contains(dirs, downloadsDir(inst)) {
		dirs = append(dirs, downloadsDir(inst))
	}
	return dirs
}

func (a *aggregator) findDownload(name string) (string, os.FileInfo, error) {
	latest := name == "" || name == "latest"
	// Playwright MCP's own snapshots and screenshots share the directory; served
	// raw from /files they would carry what masking took out of the results.
	if !latest && (!bareDownloadName(name) || autoArtifact.MatchString(name)) {
		return "", nil, errNoDownload
	}
	now := time.Now()
	settled := func(info os.FileInfo) bool { return now.Sub(info.ModTime()) >= downloadSettle }
	busy := false
	var bestDir string
	var best os.FileInfo
	for _, dir := range a.downloadDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !finishedDownload(e) {
				continue
			}
			if !latest && n != name || autoArtifact.MatchString(n) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			// latest keeps an unsettled file as a candidate: an older settled one is not what was asked for.
			if !latest && !settled(info) {
				busy = true
				continue
			}
			if best == nil || info.ModTime().After(best.ModTime()) {
				bestDir, best = dir, info
			}
		}
		if best != nil && !latest {
			break
		}
	}
	if best == nil {
		if busy {
			return "", nil, errDownloadBusy
		}
		return "", nil, errNoDownload
	}
	if !settled(best) {
		return "", nil, errDownloadBusy
	}
	return bestDir, best, nil
}

func (a *aggregator) download(s *agentSession, in downloadIn) (downloadOut, error) {
	dir, info, err := a.findDownload(in.Name)
	if err != nil {
		return downloadOut{}, err
	}
	f, err := os.Open(filepath.Join(dir, info.Name()))
	if err != nil {
		return downloadOut{}, errNoDownload
	}
	defer func() { _ = f.Close() }()
	// One pass over the file as it is now, so size, digest and inline text all
	// describe the same bytes even if the file is still growing, and the inline
	// copy never exceeds downloadInlineMax.
	head := make([]byte, downloadInlineMax)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return downloadOut{}, fmt.Errorf("read download: %w", err)
	}
	head = head[:n]
	sum := sha256.New()
	sum.Write(head)
	rest, err := io.Copy(sum, f)
	if err != nil {
		return downloadOut{}, fmt.Errorf("read download: %w", err)
	}
	kind := mime.TypeByExtension(filepath.Ext(info.Name()))
	if kind == "" {
		kind = http.DetectContentType(head)
	}
	handle, err := a.r.mintFile(s.principal, dir, info.Name())
	if err != nil {
		return downloadOut{}, err
	}
	out := downloadOut{
		Name: info.Name(), Size: int64(n) + rest, MIME: kind, SHA256: hex.EncodeToString(sum.Sum(nil)),
		URL: a.r.publicURL + "/files/" + handle,
	}
	if rest == 0 && n < downloadInlineMax && textual(kind) && utf8.Valid(head) {
		out.Text = string(head)
	}
	return out, nil
}

func textual(kind string) bool {
	base, _, _ := strings.Cut(kind, ";")
	return strings.HasPrefix(base, "text/") || slices.Contains([]string{"application/json", "application/xml", "application/javascript"}, base) ||
		strings.HasSuffix(base, "+json") || strings.HasSuffix(base, "+xml")
}
