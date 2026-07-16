package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/coder/websocket"
)

var errRawCDP = errors.New("cdp error")

func TestStorageStateRoundTrip(t *testing.T) {
	t.Parallel()
	srv, jar := fakeCDPServer(t)
	exec := dialExec(t, srv.wsURL)
	ctx := cdp.WithExecutor(t.Context(), exec)

	want := &StorageState{
		Cookies: []Cookie{
			{Name: "sid", Value: "abc", Domain: "example.com", Path: "/", Secure: true, HTTPOnly: true, SameSite: "Lax", Expires: 1893456000},
			{Name: "sess", Value: "z", Domain: "example.com", Path: "/", Expires: -1},
		},
	}
	ls := map[string]string{"token": "t1", "theme": "dark"}

	if err := setCookies(ctx, toCookieParams(want.Cookies)); err != nil {
		t.Fatalf("setCookies: %v", err)
	}
	if err := writeLocalStorage(ctx, ls); err != nil {
		t.Fatalf("writeLocalStorage: %v", err)
	}

	gotCookies, err := getAllCookies(ctx)
	if err != nil {
		t.Fatalf("getAllCookies: %v", err)
	}
	round := fromCDPCookies(gotCookies)
	if len(round) != len(want.Cookies) {
		t.Fatalf("cookie count: got %d want %d", len(round), len(want.Cookies))
	}
	byName := map[string]Cookie{}
	for _, c := range round {
		byName[c.Name] = c
	}
	for _, w := range want.Cookies {
		g, ok := byName[w.Name]
		if !ok {
			t.Fatalf("missing cookie %q", w.Name)
		}
		if g.Value != w.Value || g.Domain != w.Domain || g.Secure != w.Secure || g.HTTPOnly != w.HTTPOnly {
			t.Fatalf("cookie %q mismatch: got %+v want %+v", w.Name, g, w)
		}
	}

	gotLS, err := readLocalStorage(ctx)
	if err != nil {
		t.Fatalf("readLocalStorage: %v", err)
	}
	if len(gotLS) != len(ls) {
		t.Fatalf("localStorage: got %v want %v", gotLS, ls)
	}
	for k, v := range ls {
		if gotLS[k] != v {
			t.Fatalf("localStorage[%q]=%q want %q", k, gotLS[k], v)
		}
	}
	_ = jar
}

func TestBrowserWSURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" || r.URL.Query().Get("fingerprint") != "linkedin" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"webSocketDebuggerUrl": "ws://" + r.Host + "/fingerprint/linkedin/devtools/browser/abc",
		})
	}))
	defer srv.Close()

	got, err := browserWSURL(t.Context(), srv.URL, "linkedin")
	if err != nil {
		t.Fatalf("browserWSURL: %v", err)
	}
	if !strings.HasSuffix(got, "/fingerprint/linkedin/devtools/browser/abc") {
		t.Fatalf("ws url: %q", got)
	}
}

func TestBrowserWSURLMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Browser": "Chrome/148"})
	}))
	defer srv.Close()
	if _, err := browserWSURL(t.Context(), srv.URL, "x"); !errors.Is(err, errNoWSURL) {
		t.Fatalf("want errNoWSURL, got %v", err)
	}
}

func TestLSWriteExprRecoverable(t *testing.T) {
	t.Parallel()
	expr := lsWriteExpr(map[string]string{"a": "1"})
	payload := strings.TrimSuffix(strings.TrimPrefix(expr, lsWritePrefix), lsWriteSuffix)
	var m map[string]string
	if err := json.Unmarshal([]byte(payload), &m); err != nil || m["a"] != "1" {
		t.Fatalf("payload=%q m=%v err=%v", payload, m, err)
	}
}

// ---------------------------------------------------------------------------
// fake CDP endpoint
// ---------------------------------------------------------------------------

type fakeServer struct {
	wsURL string
}

type cookieJar struct {
	mu      sync.Mutex
	cookies []map[string]any
	ls      map[string]string
}

// fakeCDPServer speaks just enough CDP over a WebSocket to round-trip cookies and
// localStorage: it stores whatever setCookies/localStorage-write frames set and
// replays them on getAllCookies / localStorage-read.
func fakeCDPServer(t *testing.T) (*fakeServer, *cookieJar) {
	t.Helper()
	jar := &cookieJar{ls: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			reply := jar.handle(data)
			if err := conn.Write(r.Context(), websocket.MessageText, reply); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	ws := "ws" + strings.TrimPrefix(srv.URL, "http")
	return &fakeServer{wsURL: ws}, jar
}

func (j *cookieJar) handle(data []byte) []byte {
	var msg struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params struct {
			Cookies    []map[string]any `json:"cookies"`
			Expression string           `json:"expression"`
		} `json:"params"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return []byte(`{"id":0,"result":{}}`)
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	result := map[string]any{}
	switch msg.Method {
	case "Network.setCookies":
		j.cookies = append(j.cookies, msg.Params.Cookies...)
	case "Storage.getCookies":
		result["cookies"] = j.cookies
	case "Runtime.evaluate":
		result["result"] = j.evaluate(msg.Params.Expression)
	}
	out, _ := json.Marshal(map[string]any{"id": msg.ID, "result": result})
	return out
}

func (j *cookieJar) evaluate(expr string) map[string]any {
	switch {
	case strings.HasPrefix(expr, lsWritePrefix):
		payload := strings.TrimSuffix(strings.TrimPrefix(expr, lsWritePrefix), lsWriteSuffix)
		var m map[string]string
		if json.Unmarshal([]byte(payload), &m) == nil {
			maps.Copy(j.ls, m)
		}
		return map[string]any{}
	case expr == lsReadExpr:
		return map[string]any{"value": j.ls}
	default:
		return map[string]any{}
	}
}

// ---------------------------------------------------------------------------
// raw CDP executor over one WebSocket (test transport for the low-level fns)
// ---------------------------------------------------------------------------

type rawExecutor struct {
	conn *websocket.Conn
	mu   sync.Mutex
	id   int64
}

func dialExec(t *testing.T, wsURL string) *rawExecutor {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial fake CDP: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return &rawExecutor{conn: conn}
}

func (e *rawExecutor) Execute(ctx context.Context, method string, params, res any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.id++
	p := json.RawMessage("{}")
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		p = b
	}
	env, err := json.Marshal(struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{e.id, method, p})
	if err != nil {
		return err
	}
	if err := e.conn.Write(ctx, websocket.MessageText, env); err != nil {
		return err
	}
	for {
		_, data, err := e.conn.Read(ctx)
		if err != nil {
			return err
		}
		var reply struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &reply) != nil || reply.ID != e.id {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("%w: %s", errRawCDP, reply.Error.Message)
		}
		if res != nil && len(reply.Result) > 0 {
			return json.Unmarshal(reply.Result, res)
		}
		return nil
	}
}
