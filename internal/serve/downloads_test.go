package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// downloadsPool launches seed s1 on a fake launcher and seeds its Downloads dir
// with files. Returns the multiplexer and the instance's download dir.
func downloadsPool(t *testing.T, files map[string]string) (*multiplexer, string) {
	t.Helper()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())
	inst, err := pool.getOrLaunch(context.Background(), connectRequest{seed: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	dir := downloadsDir(inst)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("spawn should create the downloads dir: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &multiplexer{pool: pool, port: 9222}, dir
}

func downloadsReq(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Host = "127.0.0.1:9222"
	if rest, ok := strings.CutPrefix(r.URL.Path, "/downloads/"); ok && rest != "" {
		name, err := url.PathUnescape(rest)
		if err != nil {
			name = rest
		}
		r.SetPathValue("name", name)
	}
	return r
}

func TestDownloadsListFiltersAndSorts(t *testing.T) {
	t.Parallel()
	m, dir := downloadsPool(t, map[string]string{
		"creds.json":         `{"secret":true}`,
		".hidden":            "x",
		"partial.crdownload": "x",
		"report (1).pdf":     "pdf",
	})
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	m.handleDownloadsList(rec, downloadsReq("/downloads?fingerprint=s1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Downloads []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"downloads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Downloads) != 2 {
		t.Fatalf("want 2 completed downloads, got %+v", payload.Downloads)
	}
	for _, d := range payload.Downloads {
		if d.Name == ".hidden" || d.Name == "partial.crdownload" || d.Name == "sub" {
			t.Errorf("filtered entry leaked into listing: %q", d.Name)
		}
	}
}

func TestDownloadsGetStreamsFile(t *testing.T) {
	t.Parallel()
	m, _ := downloadsPool(t, map[string]string{"creds.json": `{"client_secret":"x"}`})

	rec := httptest.NewRecorder()
	m.handleDownloadsGet(rec, downloadsReq("/downloads/creds.json?fingerprint=s1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"client_secret":"x"}` {
		t.Errorf("body=%q", rec.Body.String())
	}
	// Untrusted content must come back opaque, never sniffed/rendered.
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type=%q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
}

func TestDownloadsGetRejectsTraversalAndDotfiles(t *testing.T) {
	t.Parallel()
	m, _ := downloadsPool(t, map[string]string{"ok.txt": "fine"})

	for _, name := range []string{"../secret", "..", ".", "a/b", ".hidden", ""} {
		rec := httptest.NewRecorder()
		req := downloadsReq("/downloads/x?fingerprint=s1")
		req.SetPathValue("name", name)
		m.handleDownloadsGet(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("name %q: status=%d want 400", name, rec.Code)
		}
	}
}

func TestDownloadsSeedNotRunning(t *testing.T) {
	t.Parallel()
	m, _ := downloadsPool(t, nil)
	for _, target := range []string{"/downloads?fingerprint=other", "/downloads"} {
		rec := httptest.NewRecorder()
		m.handleDownloadsList(rec, downloadsReq(target))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status=%d want 404", target, rec.Code)
		}
	}
}

func TestDownloadsMissingFileIs404(t *testing.T) {
	t.Parallel()
	m, _ := downloadsPool(t, nil)
	rec := httptest.NewRecorder()
	m.handleDownloadsGet(rec, downloadsReq("/downloads/nope.bin?fingerprint=s1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}

func TestDownloadsRejectNonLoopbackHost(t *testing.T) {
	t.Parallel()
	m, _ := downloadsPool(t, map[string]string{"creds.json": "secret"})
	for _, target := range []string{"/downloads?fingerprint=s1", "/downloads/creds.json?fingerprint=s1"} {
		rec := httptest.NewRecorder()
		req := downloadsReq(target)
		req.Host = "attacker.com:9222"
		if strings.Contains(target, "creds.json") {
			m.handleDownloadsGet(rec, req)
		} else {
			m.handleDownloadsList(rec, req)
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status=%d want 403", target, rec.Code)
		}
	}
}

func TestDownloadsDefaultSeedRouting(t *testing.T) {
	t.Parallel()
	fl := &fakeLauncher{port: 5100}
	pool := newTestPool(t, serveConfig{}, fl.toLauncher())
	inst, err := pool.getOrLaunch(context.Background(), connectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloadsDir(inst), "d.txt"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &multiplexer{pool: pool, port: 9222}

	rec := httptest.NewRecorder()
	m.handleDownloadsGet(rec, downloadsReq("/downloads/d.txt"))
	if rec.Code != http.StatusOK || rec.Body.String() != "v" {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestSeedProfileDefaultsPinsDownloadDir(t *testing.T) {
	t.Parallel()
	readPref := func(userDataDir string) (string, bool) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(userDataDir, "Default", "Preferences"))
		if err != nil {
			t.Fatalf("read Preferences: %v", err)
		}
		var prefs struct {
			Download struct {
				DefaultDirectory  string `json:"default_directory"`
				PromptForDownload bool   `json:"prompt_for_download"`
			} `json:"download"`
		}
		if err := json.Unmarshal(b, &prefs); err != nil {
			t.Fatalf("unmarshal Preferences: %v", err)
		}
		return prefs.Download.DefaultDirectory, prefs.Download.PromptForDownload
	}

	// Fresh profile: download dir created and pinned, prompt disabled.
	fresh := t.TempDir()
	seedProfileDefaults(fresh)
	want := filepath.Join(fresh, downloadsDirName)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("download dir not created: %v", err)
	}
	if dir, prompt := readPref(fresh); dir != want || prompt {
		t.Errorf("fresh pin: dir=%q prompt=%v, want dir=%q prompt=false", dir, prompt, want)
	}

	// Existing profile with unrelated prefs: pin is merged in, search pref kept.
	existing := t.TempDir()
	def := filepath.Join(existing, "Default")
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	prior := `{"default_search_provider":{"enabled":true},"profile":{"name":"keep me"}}`
	if err := os.WriteFile(filepath.Join(def, "Preferences"), []byte(prior), 0o600); err != nil {
		t.Fatal(err)
	}
	seedProfileDefaults(existing)
	if dir, prompt := readPref(existing); dir != filepath.Join(existing, downloadsDirName) || prompt {
		t.Errorf("existing pin: dir=%q prompt=%v", dir, prompt)
	}
	b, _ := os.ReadFile(filepath.Join(def, "Preferences"))
	if !strings.Contains(string(b), "keep me") {
		t.Errorf("existing prefs clobbered: %s", b)
	}
}
