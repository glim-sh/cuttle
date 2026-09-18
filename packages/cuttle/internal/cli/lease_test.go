package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hostCurl runs the in-container curl on this host, aimed at a test server in
// place of the daemon.
type hostCurl struct{ base string }

func (h hostCurl) ExecCommand(_ string, argv []string) (string, []string) {
	args := make([]string, 0, len(argv)-1)
	for _, a := range argv[1:] {
		args = append(args, strings.Replace(a, playwrightCDPEndpoint, h.base, 1))
	}
	return argv[0], args
}

// leaseStub records the requests it sees and answers each with reply.
type leaseStub struct {
	mu    sync.Mutex
	seen  []string
	reply func(r *http.Request) (int, string)
}

func (s *leaseStub) start(t *testing.T) hostCurl {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.RequestURI())
		s.mu.Unlock()
		code, body := s.reply(r)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return hostCurl{base: srv.URL}
}

func (s *leaseStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

const heldBody = `{"held":true,"owner":"jev-browse 41@host","age_seconds":42,"ttl_seconds":120}`

func TestPlaywrightReadOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"snapshot"}, true},
		{[]string{"console", "error"}, true},
		{[]string{"tab-list"}, true},
		{[]string{"click", "--help"}, true},
		{[]string{"--version"}, true},
		{[]string{"-s=cuttle", "snapshot"}, true},
		{[]string{"click", "e5"}, false},
		{[]string{"fill", "e5", "snapshot"}, false},
		{[]string{"goto", "https://example.com"}, false},
		{[]string{"eval", "document.title"}, false},
		{[]string{"attach"}, false},
		{[]string{"some-future-verb"}, false},
	}
	for _, tt := range tests {
		if got := playwrightReadOnly(tt.args); got != tt.want {
			t.Errorf("playwrightReadOnly(%v)=%v want %v", tt.args, got, tt.want)
		}
	}
}

func TestGatePlaywright(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		takeover bool
		code     int
		body     string
		wantErr  bool
		wantSeen string // the one request expected, or "" for none
	}{
		{name: "held refuses a mutating verb", args: []string{"fill", "e5", "hunter2"}, code: 200, body: heldBody, wantErr: true, wantSeen: "GET /lease"},
		{name: "held lets a read verb through without asking", args: []string{"snapshot"}, code: 200, body: heldBody},
		{name: "a free lease passes", args: []string{"click", "e5"}, code: 200, body: `{"held":false}`, wantSeen: "GET /lease"},
		{name: "a daemon without leases passes", args: []string{"click", "e5"}, code: 404, body: "404 page not found", wantSeen: "GET /lease"},
		{name: "takeover force-releases and proceeds", args: []string{"click", "e5"}, takeover: true, code: 200, body: `{"status":"ok"}`, wantSeen: "DELETE /lease?force=true&owner=cuttle+pw+"},
		{name: "takeover in front of a read verb evicts nobody", args: []string{"snapshot"}, takeover: true, code: 200, body: heldBody},
		{name: "takeover in front of a help request evicts nobody", args: []string{"click", "--help"}, takeover: true, code: 200, body: heldBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stub := &leaseStub{reply: func(*http.Request) (int, string) { return tt.code, tt.body }}
			ex := stub.start(t)
			err := gatePlaywright(context.Background(), ex, tt.args, tt.takeover)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tt.wantErr)
			}
			if err != nil {
				msg := err.Error()
				if !errors.Is(err, errSessionLeased) || !strings.Contains(msg, "jev-browse 41@host") ||
					!strings.Contains(msg, "42s") || !strings.Contains(msg, flagTakeover) {
					t.Fatalf("refusal should name holder, age and the takeover flag: %q", msg)
				}
				if strings.Contains(msg, "hunter2") {
					t.Fatalf("refusal must not echo the args: %q", msg)
				}
			}
			seen := stub.requests()
			if tt.wantSeen == "" {
				if len(seen) != 0 {
					t.Fatalf("expected no lease request, saw %v", seen)
				}
				return
			}
			if len(seen) != 1 || !strings.HasPrefix(seen[0], tt.wantSeen) {
				t.Fatalf("requests=%v want one starting %q", seen, tt.wantSeen)
			}
		})
	}
}

func TestAcquireLease(t *testing.T) {
	t.Parallel()
	t.Run("conflict names holder, age and takeover", func(t *testing.T) {
		t.Parallel()
		stub := &leaseStub{reply: func(*http.Request) (int, string) { return http.StatusConflict, heldBody }}
		_, err := acquireLease(context.Background(), stub.start(t), "jev-browse 7@me", false)
		if !errors.Is(err, errSessionLeased) || !strings.Contains(err.Error(), "jev-browse 41@host (for 42s)") ||
			!strings.Contains(err.Error(), flagTakeover) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("grant carries token and ttl", func(t *testing.T) {
		t.Parallel()
		stub := &leaseStub{reply: func(*http.Request) (int, string) {
			return http.StatusOK, `{"owner":"jev-browse 7@me","token":"abc","ttl_seconds":120}`
		}}
		l, err := acquireLease(context.Background(), stub.start(t), "jev-browse 7@me", false)
		if err != nil || l.token != "abc" || l.ttl != 120*time.Second {
			t.Fatalf("lease=%+v err=%v", l, err)
		}
		l.release()
		if seen := stub.requests(); len(seen) != 2 || seen[1] != "DELETE /lease?token=abc" {
			t.Fatalf("release should DELETE with the token: %v", seen)
		}
	})
	t.Run("takeover force-releases before acquiring", func(t *testing.T) {
		t.Parallel()
		stub := &leaseStub{reply: func(*http.Request) (int, string) {
			return http.StatusOK, `{"token":"abc","ttl_seconds":120}`
		}}
		if _, err := acquireLease(context.Background(), stub.start(t), "jev-browse 7@me", true); err != nil {
			t.Fatal(err)
		}
		seen := stub.requests()
		if len(seen) != 2 || !strings.HasPrefix(seen[0], "DELETE /lease?force=true") || !strings.HasPrefix(seen[1], "POST /lease") {
			t.Fatalf("requests=%v", seen)
		}
	})
	t.Run("a daemon without leases runs unleased", func(t *testing.T) {
		t.Parallel()
		stub := &leaseStub{reply: func(*http.Request) (int, string) { return http.StatusNotFound, "404 page not found" }}
		l, err := acquireLease(context.Background(), stub.start(t), "jev-browse 7@me", false)
		if err != nil || l.token != "" {
			t.Fatalf("lease=%+v err=%v", l, err)
		}
		l.release()
		if seen := stub.requests(); len(seen) != 1 {
			t.Fatalf("an unleased run must not release: %v", seen)
		}
	})
}

func TestLeaseHeartbeatStopsTheRunOnTakeover(t *testing.T) {
	t.Parallel()
	stub := &leaseStub{reply: func(r *http.Request) (int, string) {
		if r.URL.Query().Get("token") != "abc" {
			return http.StatusBadRequest, `{}`
		}
		return http.StatusConflict, `{"owner":"cuttle pw 9@there","age_seconds":0}`
	}}
	l := &sessionLease{ex: stub.start(t), owner: "jev-browse 7@me", token: "abc", ttl: 90 * time.Millisecond}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go l.heartbeat(ctx, cancel)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("heartbeat never noticed the takeover")
	}
	cause := context.Cause(ctx)
	if !errors.Is(cause, errSessionTakenOver) || !strings.Contains(cause.Error(), "cuttle pw 9@there") {
		t.Fatalf("cause=%v", cause)
	}
}

func TestLeaseGuardStopsBeforeTheNextActionOnTakeover(t *testing.T) {
	t.Parallel()
	stub := &leaseStub{reply: func(*http.Request) (int, string) {
		return http.StatusConflict, `{"owner":"cuttle pw 9@there","age_seconds":0}`
	}}
	l := &sessionLease{ex: stub.start(t), owner: "jev-browse 7@me", token: "abc", ttl: time.Hour}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var ran []string
	drive := l.guard(func(_ context.Context, args ...string) (string, error) {
		ran = append(ran, args[0])
		return "", nil
	}, cancel)

	if _, err := drive(ctx, "snapshot"); err != nil || len(stub.requests()) != 0 {
		t.Fatalf("a read verb should pass without a lease call: err=%v requests=%v", err, stub.requests())
	}
	if _, err := drive(ctx, "click", "e3"); !errors.Is(err, errSessionTakenOver) {
		t.Fatalf("err=%v, want the takeover", err)
	}
	if len(ran) != 1 || ran[0] != "snapshot" {
		t.Fatalf("the click must not run after a takeover: ran=%v", ran)
	}
	if cause := context.Cause(ctx); !strings.Contains(cause.Error(), "cuttle pw 9@there") {
		t.Fatalf("cause=%v", cause)
	}
}

// TestLeaseHeartbeatAndGuardShareOneLease drives the two goroutines a real run
// has on one sessionLease - the heartbeat and the per-verb guard - at once, so
// -race covers the sharing, and so a takeover still ends the run when both are
// renewing. Meant to be run under -race.
func TestLeaseHeartbeatAndGuardShareOneLease(t *testing.T) {
	t.Parallel()
	var taken atomic.Bool
	stub := &leaseStub{reply: func(*http.Request) (int, string) {
		if taken.Load() {
			return http.StatusConflict, `{"owner":"cuttle pw 9@there"}`
		}
		return http.StatusOK, `{"owner":"jev-browse 7@me","token":"abc","ttl_seconds":120}`
	}}
	l := &sessionLease{ex: stub.start(t), owner: "jev-browse 7@me", token: "abc", ttl: 120 * time.Millisecond}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	go l.heartbeat(ctx, cancel)

	var drives atomic.Int64
	drive := l.guard(func(_ context.Context, _ ...string) (string, error) {
		drives.Add(1)
		return "", nil
	}, cancel)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 10 {
				if _, err := drive(ctx, "click", "e5"); err != nil {
					t.Errorf("a verb was refused while the lease was held: %v", err)
					return
				}
				if _, err := drive(ctx, "snapshot"); err != nil {
					t.Errorf("a read verb was refused: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	if got := drives.Load(); got != 80 {
		t.Fatalf("ran %d verbs, want 80", got)
	}

	taken.Store(true)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("neither the heartbeat nor the guard noticed the takeover")
	}
	if cause := context.Cause(ctx); !errors.Is(cause, errSessionTakenOver) {
		t.Fatalf("cause=%v, want the takeover", cause)
	}
}
