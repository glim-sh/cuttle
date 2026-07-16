package profile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/cdp"
)

const testCDPBase = "http://127.0.0.1:9222"

var errInjectBoom = errors.New("inject boom")

func TestRunCheckpointsTicksThenStops(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	ticks := make(chan struct{}, 16)
	done := make(chan struct{})
	go func() {
		runCheckpoints(ctx, 5*time.Millisecond, func() { ticks <- struct{}{} })
		close(done)
	}()
	for i := range 3 {
		select {
		case <-ticks:
		case <-time.After(2 * time.Second):
			t.Fatalf("checkpoint tick %d did not fire", i)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runCheckpoints did not stop after cancel")
	}
}

func TestCheckoutInjectsAndCheckinSaves(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const name = "linkedin"

	initial := &cdp.StorageState{
		Cookies: []cdp.Cookie{{Name: "old", Value: "1", Domain: exampleDomain, Path: "/", Expires: -1}},
	}
	if err := saveState(DataDir(name), initial); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	var injected atomic.Value
	inject := func(_ context.Context, _, seed string, st *cdp.StorageState) error {
		if seed != name {
			t.Errorf("inject seed=%q want %q", seed, name)
		}
		injected.Store(st)
		return nil
	}
	updated := &cdp.StorageState{
		Cookies: []cdp.Cookie{{Name: "new", Value: "2", Domain: exampleDomain, Path: "/", Expires: -1}},
	}
	extract := func(_ context.Context, _, _ string, _ []string) (*cdp.StorageState, error) {
		return updated, nil
	}

	s, err := checkoutSession(t.Context(), Options{Name: name, CDPBase: testCDPBase, Interval: time.Hour}, inject, extract)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	got, ok := injected.Load().(*cdp.StorageState)
	if !ok || got == nil || len(got.Cookies) != 1 || got.Cookies[0].Name != "old" {
		t.Fatalf("inject received %+v, want the seeded state", got)
	}

	if cerr := s.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}

	final, err := loadState(DataDir(name))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(final.Cookies) != 1 || final.Cookies[0].Name != "new" {
		t.Fatalf("checkin wrote %+v, want the extracted state", final.Cookies)
	}

	if _, serr := os.Stat(filepath.Join(DataDir(name), lockName)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("lock not released: %v", serr)
	}
}

func TestCheckoutRejectsSecondSession(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	nop := func(context.Context, string, string, *cdp.StorageState) error { return nil }
	ext := func(context.Context, string, string, []string) (*cdp.StorageState, error) {
		return &cdp.StorageState{}, nil
	}
	opts := Options{Name: "x", CDPBase: testCDPBase, Interval: time.Hour}

	s1, err := checkoutSession(t.Context(), opts, nop, ext)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	if _, serr := checkoutSession(t.Context(), opts, nop, ext); !errors.Is(serr, errCheckedOut) {
		t.Fatalf("second checkout: want errCheckedOut, got %v", serr)
	}
	if cerr := s1.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
}

func TestCheckoutInjectFailureReleasesLock(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	inject := func(context.Context, string, string, *cdp.StorageState) error { return errInjectBoom }
	ext := func(context.Context, string, string, []string) (*cdp.StorageState, error) {
		return &cdp.StorageState{}, nil
	}
	opts := Options{Name: "x", CDPBase: testCDPBase}
	if _, err := checkoutSession(t.Context(), opts, inject, ext); !errors.Is(err, errInjectBoom) {
		t.Fatalf("want inject error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(DataDir("x"), lockName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock should be released after inject failure: %v", err)
	}
}

func TestRemoteSessionIsInert(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	inject := func(context.Context, string, string, *cdp.StorageState) error {
		t.Error("remote session must not inject")
		return nil
	}
	ext := func(context.Context, string, string, []string) (*cdp.StorageState, error) {
		t.Error("remote session must not extract")
		return nil, nil
	}
	s, err := checkoutSession(t.Context(), Options{Name: "auto", Remote: true}, inject, ext)
	if err != nil {
		t.Fatalf("remote checkout: %v", err)
	}
	if cerr := s.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	if _, serr := os.Stat(filepath.Join(DataDir("auto"), lockName)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("remote session should not create a lock: %v", serr)
	}
}
