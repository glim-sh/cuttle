package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/glim-sh/cuttle/internal/backend"
	"github.com/glim-sh/cuttle/internal/cdp"
	"github.com/glim-sh/cuttle/internal/profile"
)

// injectLocalCanonicalState must restore a saved login only into a seed the
// daemon lacks (GET 404), and never clobber a seed the daemon already holds
// (GET 200) - that is what keeps a normal restart's newer snapshot authoritative.
func TestInjectLocalCanonicalStateRestoresOnlyMissing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // no configured profiles => all local

	login := &cdp.StorageState{
		Cookies: []cdp.Cookie{{Name: "sid", Value: "v", Domain: "example.com", Path: "/", Expires: -1}},
	}
	for _, n := range []string{"missing", "present"} {
		if err := profile.SaveState(n, login); err != nil {
			t.Fatalf("SaveState %s: %v", n, err)
		}
	}

	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seed := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/profile/"), "/state")
		switch r.Method {
		case http.MethodGet:
			if seed == "present" { // daemon already has this one
				w.Header().Set("ETag", `"x"`)
				_ = json.NewEncoder(w).Encode(&cdp.StorageState{})
				return
			}
			http.Error(w, "no state", http.StatusNotFound)
		case http.MethodPut:
			puts = append(puts, seed)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "etag": `"y"`})
		}
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	ep := backend.Endpoint{CDPHost: u.Hostname(), CDPPort: port}

	injectLocalCanonicalState(context.Background(), io.Discard, ep)

	if !slices.Equal(puts, []string{"missing"}) {
		t.Fatalf("expected a PUT only for the daemon-missing seed, got %v", puts)
	}
}
