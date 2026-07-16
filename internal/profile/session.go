package profile

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/glim-sh/cuttle/internal/cdp"
)

// defaultCheckpointInterval is how often a live local-canonical session writes
// the snowballed cookie/localStorage delta back, so a crash before checkin loses
// at most this much.
const defaultCheckpointInterval = 5 * time.Minute

// checkpointTimeout bounds one extract+save so a wedged browser cannot stall the
// checkpoint loop forever.
const checkpointTimeout = 30 * time.Second

// Options configures a profile session.
type Options struct {
	Name     string        // profile name == seed
	CDPBase  string        // e.g. http://127.0.0.1:9222
	Remote   bool          // storage = "remote": durable remote dir, no checkout/checkin
	Interval time.Duration // checkpoint interval; 0 uses the default
}

// injectFunc and extractFunc are the CDP operations, injectable so the session
// is testable without a real browser.
type (
	injectFunc  func(ctx context.Context, cdpBase, seed string, st *cdp.StorageState) error
	extractFunc func(ctx context.Context, cdpBase, seed string, origins []string) (*cdp.StorageState, []string, error)
)

// Session is a live checkout of a local-canonical profile into a remote seed.
// For a remote-persistent profile it is an inert handle (the remote dir is
// durable, so there is nothing to check out or in).
type Session struct {
	opts    Options
	dir     string
	lock    *lock
	origins []string

	inject  injectFunc
	extract extractFunc

	loopCancel context.CancelFunc
	loopDone   chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// Checkout starts a session: for a local-canonical profile it takes the
// single-writer lock, injects the local storage_state into the fresh remote
// seed, and begins periodic checkpoints. For a remote-persistent profile it
// returns an inert session. The caller MUST Close the returned session (a
// deferred Close plus a signal-aware context covers Ctrl-C).
func Checkout(ctx context.Context, opts Options) (*Session, error) {
	return checkoutSession(ctx, opts, cdp.Inject, cdp.Extract)
}

func checkoutSession(ctx context.Context, opts Options, inject injectFunc, extract extractFunc) (*Session, error) {
	if err := checkName(opts.Name); err != nil {
		return nil, err
	}
	s := &Session{opts: opts, dir: DataDir(opts.Name), inject: inject, extract: extract}
	if opts.Remote {
		return s, nil
	}

	lk, err := acquireLock(s.dir)
	if err != nil {
		return nil, err
	}
	s.lock = lk

	st, err := loadState(s.dir)
	if err != nil {
		_ = lk.release()
		return nil, err
	}
	s.origins = candidateOrigins(st)
	if err := inject(ctx, opts.CDPBase, opts.Name, st); err != nil {
		_ = lk.release()
		return nil, err
	}

	s.startCheckpoints()
	return s, nil
}

func (s *Session) interval() time.Duration {
	if s.opts.Interval > 0 {
		return s.opts.Interval
	}
	return defaultCheckpointInterval
}

// startCheckpoints runs the checkpoint loop on a detached context so a
// request-scoped timeout on the checkout ctx cannot kill periodic saves; Close
// stops it.
func (s *Session) startCheckpoints() {
	ctx, cancel := context.WithCancel(context.Background())
	s.loopCancel = cancel
	s.loopDone = make(chan struct{})
	go func() {
		defer close(s.loopDone)
		runCheckpoints(ctx, s.interval(), func() { _ = s.checkpoint() })
	}()
}

// checkpoint extracts the current storage_state and writes it back atomically.
// It is best-effort: an extract failure (e.g. a transiently unreachable browser)
// is returned for logging but never aborts the session. Origins that failed to
// load this pass keep their last-known localStorage (carryForwardLocalStorage),
// so a transient blip does not silently drop persisted state from the canonical
// file on the unconditional overwrite.
func (s *Session) checkpoint() error {
	ctx, cancel := context.WithTimeout(context.Background(), checkpointTimeout)
	defer cancel()
	st, failed, err := s.extract(ctx, s.opts.CDPBase, s.opts.Name, s.origins)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		st = carryForwardLocalStorage(s.dir, st, failed)
	}
	return saveState(s.dir, st)
}

// Close checks the session in: it stops the checkpoint loop, performs a final
// extract+save, and releases the lock. Releasing the lock always happens even if
// the final save fails, so a crashed browser never wedges the profile. Close is
// idempotent.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		if s.opts.Remote {
			return
		}
		if s.loopCancel != nil {
			s.loopCancel()
			<-s.loopDone
		}
		saveErr := s.checkpoint()
		relErr := s.lock.release()
		s.closeErr = errors.Join(saveErr, relErr)
	})
	return s.closeErr
}

// runCheckpoints calls fn on every interval tick until ctx is cancelled.
func runCheckpoints(ctx context.Context, interval time.Duration, fn func()) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
