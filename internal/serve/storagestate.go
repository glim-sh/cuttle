package serve

import (
	"net/url"
	"strings"

	"github.com/glim-sh/cuttle/internal/cdp"
)

// The two pure helpers the daemon's capture/re-inject path needs: which origins
// to re-read localStorage from, and how to merge a partial capture over the prior
// snapshot without dropping state.

// carryForward re-attaches prior localStorage for origins that failed to load
// this pass, so an unconditional overwrite never drops persisted state on a
// transient per-origin blip. A nil prior carries nothing forward.
func carryForward(prior, st *cdp.StorageState, failed []string) *cdp.StorageState {
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

// candidateOrigins is the set of origins a capture re-reads localStorage from:
// origins already recorded in the state, plus https origins derived from cookie
// domains, so a fresh login's localStorage is captured even before its origin is
// first recorded. localStorage is origin-scoped, so unknown origins cannot be
// discovered without visiting them. Nil-safe.
func candidateOrigins(st *cdp.StorageState) []string {
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
