// Package profile keeps a browser profile's auth state (cookies + per-origin
// localStorage) canonical on the local machine and checks it in and out of an
// otherwise-ephemeral remote browser seed over CDP.
//
// A named profile is a cuttle seed. Its storage_state.json lives under
// $XDG_DATA_HOME/cuttle/profiles/<name>/. On session start the state is injected
// into the fresh remote seed; during and at the end of the session it is
// extracted back and written atomically, so a crash loses at most one checkpoint
// interval of cookie deltas. "Resides locally" is true at rest; a live session
// necessarily holds the cookies on the remote to act as the user.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/glim-sh/cuttle/internal/atomicfile"
	"github.com/glim-sh/cuttle/internal/cdp"
	"github.com/glim-sh/cuttle/internal/fingerprint"
	"github.com/glim-sh/cuttle/internal/xdg"
)

var (
	errInvalidName = errors.New("invalid profile name (allowed: letters, digits, '_' and '-', 1-128 chars)")
	errReserved    = errors.New("profile name is reserved")
)

// ValidName reports whether name is a legal profile name. A profile name is a
// cuttle seed, so it shares the seed grammar (fingerprint.ValidSeed).
func ValidName(name string) bool {
	return fingerprint.ValidSeed(name)
}

func checkName(name string) error {
	if name == fingerprint.ReservedSeed {
		return fmt.Errorf("%w: %q", errReserved, name)
	}
	if !fingerprint.ValidSeed(name) {
		return fmt.Errorf("%w: %q", errInvalidName, name)
	}
	return nil
}

// DataDir is $XDG_DATA_HOME/cuttle/profiles/<name>, falling back to
// ~/.local/share.
func DataDir(name string) string {
	return filepath.Join(xdg.DataDir(), "cuttle", "profiles", name)
}

func statePath(dir string) string { return filepath.Join(dir, "storage_state.json") }

// loadState reads a profile's storage_state.json. A missing file yields an empty
// state (a brand-new profile), not an error.
func loadState(dir string) (*cdp.StorageState, error) {
	data, err := os.ReadFile(statePath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return &cdp.StorageState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading profile state: %w", err)
	}
	st := &cdp.StorageState{}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parsing profile state: %w", err)
	}
	return st, nil
}

// writeState writes storage_state.json atomically so a crash mid-write never
// leaves a truncated profile.
func writeState(dir string, st *cdp.StorageState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating profile dir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding profile state: %w", err)
	}
	if err := atomicfile.Write(statePath(dir), data, 0o600); err != nil {
		return fmt.Errorf("writing profile state: %w", err)
	}
	return nil
}

// CarryForward re-attaches prior localStorage for origins that failed to load
// this pass, so an unconditional overwrite never drops persisted state on a
// transient per-origin blip. It is the in-memory core of carryForwardLocalStorage
// (which loads prior from disk first); the serve daemon calls it directly with
// the prior snapshot it already holds. A nil prior carries nothing forward.
func CarryForward(prior, st *cdp.StorageState, failed []string) *cdp.StorageState {
	if prior == nil {
		return st
	}
	priorByOrigin := make(map[string]cdp.Origin, len(prior.Origins))
	for _, o := range prior.Origins {
		priorByOrigin[o.Origin] = o
	}
	for _, origin := range failed {
		if o, ok := priorByOrigin[origin]; ok {
			st.Origins = append(st.Origins, o)
		}
	}
	return st
}

// SaveState writes a profile's storage_state.json to its local canonical dir. It
// is the entry point for the CLI's local-canonical pull (down captures a running
// seed's state into the local store) and validates the name against the seed
// grammar (reserved names rejected) so a stray key never lands in the store.
func SaveState(name string, st *cdp.StorageState) error {
	if err := checkName(name); err != nil {
		return err
	}
	return writeState(DataDir(name), st)
}

// LoadLocal reads a profile's local canonical storage_state. A missing file
// yields an empty state (not an error), so a brand-new profile reads clean. It is
// the read side of the local-canonical restore: `up` loads each profile's state
// to seed the daemon of a fresh/recreated box.
func LoadLocal(name string) (*cdp.StorageState, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	return loadState(DataDir(name))
}

// HasState reports whether a storage_state carries anything worth restoring (any
// cookie or origin). An empty state is a brand-new profile, not a login to push.
func HasState(st *cdp.StorageState) bool {
	return st != nil && (len(st.Cookies) > 0 || len(st.Origins) > 0)
}

// ListLocal returns every profile name that has a saved storage_state on disk, so
// `up` can restore each into a box that lacks it. Best-effort: an unreadable or
// missing store yields no names rather than an error. Reserved/invalid directory
// names are skipped.
func ListLocal() []string {
	root := filepath.Join(xdg.DataDir(), "cuttle", "profiles")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		if _, serr := os.Stat(statePath(DataDir(e.Name()))); serr == nil {
			names = append(names, e.Name())
		}
	}
	return names
}

// CandidateOrigins is the set of origins a checkin re-reads localStorage from:
// origins already recorded in the state, plus https origins derived from cookie
// domains, so a fresh login's localStorage is captured even before its origin is
// first recorded. localStorage is origin-scoped, so unknown origins cannot be
// discovered without visiting them. Exported so the serve daemon derives the same
// origin set when it extracts a seed's state over its own loopback CDP. Nil-safe.
func CandidateOrigins(st *cdp.StorageState) []string {
	if st == nil {
		st = &cdp.StorageState{}
	}
	seen := map[string]struct{}{}
	var out []string
	add := func(o string) {
		if o == "" {
			return
		}
		if _, ok := seen[o]; ok {
			return
		}
		seen[o] = struct{}{}
		out = append(out, o)
	}
	for _, o := range st.OriginURLs() {
		add(o)
	}
	for _, c := range st.Cookies {
		host := strings.TrimPrefix(c.Domain, ".")
		if host == "" {
			continue
		}
		add((&url.URL{Scheme: "https", Host: host}).String())
	}
	return out
}
