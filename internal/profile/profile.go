// Package profile holds the storage-state helpers the serve daemon uses to
// capture and re-inject a seed's auth state (cookies + per-origin localStorage)
// across relaunches: which origins to re-read, and how to merge a partial
// capture over the prior snapshot without dropping state.
package profile

import (
	"net/url"
	"strings"

	"github.com/glim-sh/cuttle/internal/cdp"
)

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
