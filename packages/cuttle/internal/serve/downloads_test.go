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
	return downloadsPoolOn(t, 5100, files)
}

// downloadsPoolOn is downloadsPool with the seed's browser on a given port.
func downloadsPoolOn(t *testing.T, port int, files map[string]string) (*multiplexer, string) {
	t.Helper()
	fl := &fakeLauncher{port: port}
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
	rec := httptest.NewRecorder()
	m.handleDownloadsList(rec, downloadsReq("/downloads?fingerprint=other"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d want 404", rec.Code)
	}
	// Pool mode has no default seed to fall back to: an unseeded request is a
	// 400 (seed required), not a 404 for a seed that could never exist.
	rec = httptest.NewRecorder()
	m.handleDownloadsList(rec, downloadsReq("/downloads"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unseeded in pool mode: status=%d want 400", rec.Code)
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
	pool := newTestPool(t, serveConfig{mode: modeSession}, fl.toLauncher())
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
	seedProfileDefaults(fresh, false)
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
	seedProfileDefaults(existing, false)
	if dir, prompt := readPref(existing); dir != filepath.Join(existing, downloadsDirName) || prompt {
		t.Errorf("existing pin: dir=%q prompt=%v", dir, prompt)
	}
	b, _ := os.ReadFile(filepath.Join(def, "Preferences"))
	if !strings.Contains(string(b), "keep me") {
		t.Errorf("existing prefs clobbered: %s", b)
	}
}

// The snapshot route reads only the driver's own page snapshots, and hands them
// back with the seed's held values replaced by their sentinels.
func TestSnapshotServesMaskedDriverFile(t *testing.T) {
	t.Parallel()
	m, dir := downloadsPool(t, map[string]string{"ok.txt": "fine"})
	if err := os.MkdirAll(filepath.Join(dir, ".playwright-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	const file = ".playwright-cli/page-2026-09-19T10-27-05-037Z.yml"
	snap := `- textbox "User": fake-secret-value` + "\n" + `- textbox "Pass": other`
	if err := os.WriteFile(filepath.Join(dir, file), []byte(snap), 0o600); err != nil {
		t.Fatal(err)
	}
	m.pool.secrets.put("s1", "T", []byte("fake-secret-value"), sourceStdin, secretTTLDefault)

	rec := httptest.NewRecorder()
	m.handleSnapshot(rec, downloadsReq("/snapshot?fingerprint=s1&file="+url.QueryEscape(file)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if want := `- textbox "User": {{cuttle:T}}` + "\n" + `- textbox "Pass": other`; rec.Body.String() != want {
		t.Errorf("body=%q want %q", rec.Body.String(), want)
	}

	for _, bad := range []string{
		"", "ok.txt", "../ok.txt", "/etc/passwd", ".playwright-cli/../ok.txt",
		".playwright-cli/page-1/../../ok.txt", ".playwright-cli/console-2026.log", "x/.playwright-cli/page-1.yml",
	} {
		badRec := httptest.NewRecorder()
		m.handleSnapshot(badRec, downloadsReq("/snapshot?fingerprint=s1&file="+url.QueryEscape(bad)))
		if badRec.Code != http.StatusBadRequest {
			t.Errorf("file %q: status=%d want 400", bad, badRec.Code)
		}
	}
	rec = httptest.NewRecorder()
	m.handleSnapshot(rec, downloadsReq("/snapshot?fingerprint=s1&file="+url.QueryEscape(".playwright-cli/page-1.yml")))
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing snapshot: status=%d want 404", rec.Code)
	}
}

// fakePageBrowser is a browser with one page, answering the CDP calls the
// password read makes. fields is what the page's password inputs hold; a nil
// fields makes the evaluate fail the way a page mid-navigation does.
func fakePageBrowser(t *testing.T, fields []any) int {
	t.Helper()
	_, wsURL := startCDPBrowser(t, func(cmd map[string]any) map[string]any {
		switch cmd[cdpMethod] {
		case "Target.attachToTarget":
			return map[string]any{"sessionId": "S1"}
		case "Page.getFrameTree":
			return map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F1"}}}
		case "Page.createIsolatedWorld":
			return map[string]any{"executionContextId": 7}
		case "Runtime.callFunctionOn":
			if fields == nil {
				return map[string]any{"exceptionDetails": map[string]any{"text": "Cannot find context with specified id"}}
			}
			return map[string]any{cdpResult: map[string]any{cdpValue: fields}}
		}
		return map[string]any{}
	}, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/json/version":
			_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"` + wsURL + `"}`))
		case "/json/list":
			_, _ = w.Write([]byte(`[{"id":"1","type":"page","url":"http://x.example/login"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return serverPort(t, srv)
}

// What the page's password fields hold at snapshot time is masked too: the
// driver renders a password input as a plain textbox with its value, so a
// literal the agent typed would otherwise reach the host copy in the clear.
func TestSnapshotMasksPasswordFieldValues(t *testing.T) {
	t.Parallel()
	const file = ".playwright-cli/page-2026-09-19T10-27-05-037Z.yml"
	snap := `- textbox "User": fake-secret-value` + "\n" +
		`- textbox "Pass": fake-pw-value-123` + "\n" +
		`- textbox "Pin": 1234` + "\n" +
		`- textbox "Quoted": "fa\"ke\\{v"`
	want := `- textbox "User": {{cuttle:T}}` + "\n" +
		`- textbox "Pass": ` + passwordPlaceholder + "\n" +
		`- textbox "Pin": 1234` + "\n" +
		`- textbox "Quoted": "` + passwordPlaceholder + `"`
	for name, tc := range map[string]struct {
		fields []any
		want   string
	}{
		"fields masked":     {[]any{"fake-pw-value-123", "1234", `fa"ke\{v`}, want},
		"evaluate fails":    {nil, strings.Replace(snap, "fake-secret-value", "{{cuttle:T}}", 1)},
		"no browser at all": {nil, strings.Replace(snap, "fake-secret-value", "{{cuttle:T}}", 1)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			port := 1 // nothing listens here
			if name != "no browser at all" {
				port = fakePageBrowser(t, tc.fields)
			}
			m, dir := downloadsPoolOn(t, port, nil)
			if err := os.MkdirAll(filepath.Join(dir, ".playwright-cli"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, file), []byte(snap), 0o600); err != nil {
				t.Fatal(err)
			}
			m.pool.secrets.put("s1", "T", []byte("fake-secret-value"), sourceStdin, secretTTLDefault)
			rec := httptest.NewRecorder()
			m.handleSnapshot(rec, downloadsReq("/snapshot?fingerprint=s1&file="+url.QueryEscape(file)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != tc.want {
				t.Errorf("body=%q\nwant %q", rec.Body.String(), tc.want)
			}
		})
	}
}
