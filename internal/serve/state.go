package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/glim-sh/cuttle/internal/atomicfile"
	"github.com/glim-sh/cuttle/internal/cdp"
	"github.com/glim-sh/cuttle/internal/fingerprint"
)

var errUnsafeSeedKey = errors.New("unsafe seed key")

// stateSubdir is the child of dataDir that holds the daemon's per-seed auth-state
// snapshots. It lives OUTSIDE the per-seed Chrome profile dir so a snapshot
// survives an ephemeral profile's teardown (idle-reap / clean shutdown) and a
// daemon restart, which is the whole point of local-canonical-by-default: the
// container's Chrome dirs are disposable, the snapshot is not.
const stateSubdir = "state"

// stateEntry is one seed's captured storage state plus its ETag (a content hash
// used for optimistic concurrency on the PUT API) and whether the seed was
// explicitly marked supervised via a PUT.
type stateEntry struct {
	State      *cdp.StorageState
	ETag       string
	Supervised bool
}

// persistedState is the on-disk shape of a snapshot file.
type persistedState struct {
	State      *cdp.StorageState `json:"state"`
	Supervised bool              `json:"supervised"`
}

// stateStore holds the daemon's per-seed auth-state snapshots in memory and
// mirrors them under dataDir/state so a daemon restart keeps them. It is the
// authority the mux state API and the checkpoint supervisor share.
type stateStore struct {
	dir string
	mu  sync.Mutex
	m   map[string]*stateEntry
}

// newStateStore loads any snapshots already on disk. Load is best-effort: a
// missing or unreadable dir yields an empty store rather than failing the daemon,
// because a lost snapshot is a re-login, not a crash.
func newStateStore(dataDir string) *stateStore {
	s := &stateStore{dir: filepath.Join(dataDir, stateSubdir), m: map[string]*stateEntry{}}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return s
	}
	for _, de := range entries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(s.dir, name))
		if rerr != nil {
			continue
		}
		var p persistedState
		if json.Unmarshal(data, &p) != nil || p.State == nil {
			continue
		}
		seed := strings.TrimSuffix(name, ".json")
		s.m[seed] = &stateEntry{State: p.State, ETag: etagOf(p.State), Supervised: p.Supervised}
	}
	return s
}

// etagOf is a stable content hash of a storage state, quoted per the HTTP ETag
// grammar. Two states with the same bytes share an ETag; any change rotates it,
// which is all the PUT If-Match guard needs.
func etagOf(st *cdp.StorageState) string {
	data, err := json.Marshal(st)
	if err != nil {
		return `"0"`
	}
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func (s *stateStore) get(seed string) (*stateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[seed]
	return e, ok
}

func (s *stateStore) isSupervised(seed string) bool {
	e, ok := s.get(seed)
	return ok && e.Supervised
}

// put stores a snapshot and persists it, returning the new ETag. A non-empty
// ifMatch is compared against the current ETag per RFC 9110: "*" matches any
// existing entry (and fails when none exists); a mismatch returns conflict=true
// and writes nothing (optimistic concurrency for the PUT API). A PUT sets
// supervised true; auto-capture passes false and never clears an existing
// supervised mark.
func (s *stateStore) put(seed string, st *cdp.StorageState, supervised bool, ifMatch string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.m[seed]
	if ifMatch == "*" {
		if cur == nil {
			return "", true, nil
		}
	} else if ifMatch != "" {
		curTag := ""
		if cur != nil {
			curTag = cur.ETag
		}
		if ifMatch != curTag {
			return "", true, nil
		}
	}
	sup := supervised
	if cur != nil {
		sup = sup || cur.Supervised
	}
	e := &stateEntry{State: st, ETag: etagOf(st), Supervised: sup}
	s.m[seed] = e
	if perr := s.persist(seed, e); perr != nil {
		return "", false, perr
	}
	return e.ETag, false, nil
}

// persist writes one seed's snapshot atomically so a crash mid-write never leaves
// a truncated snapshot. The seed is validated against the shared seed grammar
// first, so a store key can never contain a path separator and escape the dir.
func (s *stateStore) persist(seed string, e *stateEntry) error {
	if !fingerprint.MatchesSeedGrammar(seed) {
		return fmt.Errorf("%w: %q", errUnsafeSeedKey, seed)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	data, err := json.MarshalIndent(persistedState{State: e.State, Supervised: e.Supervised}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding snapshot: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(s.dir, seed+".json"), data, 0o600); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}
	return nil
}
