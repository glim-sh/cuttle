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

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/cdp"
	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/fingerprint"
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
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(dir, "cuttle", "profiles", name)
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

// saveState writes storage_state.json atomically (temp file in the same dir then
// rename) so a crash mid-write never leaves a truncated profile.
func saveState(dir string, st *cdp.StorageState) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating profile dir: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding profile state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".storage_state.*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp state: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp state: %w", err)
	}
	if err := os.Rename(tmpName, statePath(dir)); err != nil {
		return fmt.Errorf("committing profile state: %w", err)
	}
	return nil
}

// candidateOrigins is the set of origins a checkin re-reads localStorage from:
// origins already recorded in the state, plus https origins derived from cookie
// domains, so a fresh login's localStorage is captured even before its origin is
// first recorded. localStorage is origin-scoped, so unknown origins cannot be
// discovered without visiting them.
func candidateOrigins(st *cdp.StorageState) []string {
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
