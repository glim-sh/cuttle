package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"https://x.example*", "https://x.example/login?next=/a", true},
		{"https://x.example*", "https://y.example/login", false},
		// path.Match would fail this one: it will not let a * cross a slash, which
		// is wrong for URLs.
		{"https://x.example/*/done", "https://x.example/a/b/done", true},
		{"*dashboard*", "https://x.example/app/dashboard/home", true},
		{"exact", "exact", true},
		{"exact", "exactly", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestParsePredicate(t *testing.T) {
	// The default is "left the origin you opened", which is what finishing a
	// sign-in looks like from outside the page.
	p, err := parsePredicate("", "https://x.example/login?next=/a")
	if err != nil {
		t.Fatalf("default predicate: %v", err)
	}
	if p.kind != predGone || p.arg != "https://x.example*" || !p.implicit {
		t.Fatalf("default predicate = %+v, want gone: on the launch origin", p)
	}
	if p.holds("https://x.example/login", "", false) {
		t.Error("still on the sign-in origin must not satisfy gone:")
	}
	if !p.holds("https://app.example/home", "", false) {
		t.Error("leaving the origin must satisfy gone:")
	}

	for _, spec := range []string{"title:Dashboard", "url:https://x*", "js:1", "gone:https://x*"} {
		if _, err := parsePredicate(spec, ""); err != nil {
			t.Errorf("parsePredicate(%q): %v", spec, err)
		}
	}
	for _, spec := range []string{"nope:x", "title:", "no-colon"} {
		if _, err := parsePredicate(spec, ""); !errors.Is(err, errBadPredicate) {
			t.Errorf("parsePredicate(%q) error = %v, want errBadPredicate", spec, err)
		}
	}
	// With no URL to derive from there is no honest default to invent.
	if _, err := parsePredicate("", ""); !errors.Is(err, errBadPredicate) {
		t.Errorf("empty spec with no URL error = %v, want errBadPredicate", err)
	}
}

func TestPredicateHolds(t *testing.T) {
	if !(predicate{kind: predTitle, arg: "Dash"}).holds("", "My Dashboard", false) {
		t.Error("title: must match a substring of the title")
	}
	if !(predicate{kind: predJS, arg: "x"}).holds("", "", true) {
		t.Error("js: must follow the evaluated boolean")
	}
	if (predicate{kind: predURL, arg: "https://a*"}).holds("https://b/", "", true) {
		t.Error("url: must not be satisfied by an unrelated URL")
	}
}

// `open --until` navigates the session, raises a window and opens a viewer on
// someone's desktop. A typo in the predicate must not do all three first.
//
// The context is one that cannot resolve, so resolveInstance would fail with its
// own error: getting errBadPredicate back is what proves the parse ran BEFORE
// the session was touched. Asserting only "some error mentions the predicate"
// would pass with the parse moved back down, on any machine with a live session.
func TestOpenValidatesThePredicateBeforeTouchingAnything(t *testing.T) {
	// --context is a root persistent flag, and this test runs the subcommand on
	// its own, so the selection is set where cobra would have written it.
	withInstance(t, instanceFlags{contextName: "no-such-context-exists"})
	var out, errOut strings.Builder
	cmd := newOpenCmd()
	cmd.SetArgs([]string{"https://example.com", "--until", "bogus"})
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if !errors.Is(err, errBadPredicate) {
		t.Fatalf("error = %v, want errBadPredicate - the predicate must be parsed before the session is resolved", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a refused predicate printed %q; nothing may happen before it is parsed", out.String())
	}
}

// A browser that dies under `open --until` ends the wait at once, rather than
// after the whole timeout with a URL that no longer exists anywhere.
func TestWaitUntilNoticesADeadBrowser(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{ReadHeaderTimeout: time.Second}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			_, _ = w.Write([]byte(`[{"type":"page","url":"https://x.example/","webSocketDebuggerUrl":"ws://` + ln.Addr().String() + `/page"}]`))
			return
		}
		// The page socket dies on its first call, and the whole browser with it.
		conn, aerr := websocket.Accept(w, r, nil)
		if aerr != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
		go func() { _ = srv.Close() }()
		_ = conn.CloseNow()
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	var out strings.Builder
	start := time.Now()
	err = waitUntil(context.Background(), &out, "127.0.0.1", port, 0, predicate{kind: predTitle, arg: "never"}, time.Minute)
	if err == nil || errors.Is(err, errWaitTimeout) || !strings.Contains(err.Error(), "the page went away") {
		t.Fatalf("err = %v, want the browser-gone error", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("took %s to notice", elapsed)
	}
}

// slowPage serves one page whose first socket never answers. With stallRedial
// the upgrade of every later socket never completes either, as through an ssh
// forward whose remote end is gone.
func slowPage(t *testing.T, stallRedial bool) (int, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	srv := &http.Server{ReadHeaderTimeout: time.Second}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			_, _ = w.Write([]byte(`[{"type":"page","url":"https://x.example/","webSocketDebuggerUrl":"ws://` + ln.Addr().String() + `/page"}]`))
			return
		}
		first := dials.Add(1) == 1
		if !first && stallRedial {
			<-r.Context().Done()
			return
		}
		conn, aerr := websocket.Accept(w, r, nil)
		if aerr != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, data, rerr := conn.Read(r.Context())
			if rerr != nil {
				return
			}
			if first {
				continue
			}
			var msg struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(data, &msg)
			result := map[string]any{
				"Page.getFrameTree":        map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "f1"}}},
				"Page.createIsolatedWorld": map[string]any{"executionContextId": 1},
				"Runtime.evaluate":         map[string]any{"result": map[string]any{"value": map[string]any{"href": "https://x.example/", "title": "done"}}},
			}[msg.Method]
			reply, _ := json.Marshal(map[string]any{"id": msg.ID, "result": result})
			if conn.Write(r.Context(), websocket.MessageText, reply) != nil {
				return
			}
		}
	})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port, &dials
}

// A page too slow to build the isolated world in time is still there: the wait
// reattaches to the same target and goes on, rather than reporting it gone.
func TestWaitUntilOutlastsASlowPage(t *testing.T) {
	t.Parallel()
	port, dials := slowPage(t, false)
	var out strings.Builder
	err := waitUntil(context.Background(), &out, "127.0.0.1", port, 0, predicate{kind: predTitle, arg: "done"}, time.Minute)
	if err != nil || dials.Load() != 2 {
		t.Fatalf("err = %v after %d dials, want the condition met on the second\n%s", err, dials.Load(), out.String())
	}
}

// A reattach that never completes is the page gone, reported at once rather
// than after the whole wait.
func TestWaitUntilGivesUpOnAStalledReattach(t *testing.T) {
	t.Parallel()
	port, _ := slowPage(t, true)
	var out strings.Builder
	start := time.Now()
	err := waitUntil(context.Background(), &out, "127.0.0.1", port, 0, predicate{kind: predTitle, arg: "done"}, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "went away") || time.Since(start) > 30*time.Second {
		t.Fatalf("err = %v after %s, want the page reported gone\n%s", err, time.Since(start), out.String())
	}
}
