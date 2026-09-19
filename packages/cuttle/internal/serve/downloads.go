package serve

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// downloadsDirName is the per-seed download directory, inside the seed's
// user-data-dir so its lifecycle rides the profile's: removed with an ephemeral
// profile, preserved by keep-profile. Chrome is pinned to write here via the
// profile preference (see seedProfileDefaults / pinDownloadDir).
const downloadsDirName = "Downloads"

func downloadsDir(inst *chromeInstance) string {
	return filepath.Join(inst.userDataDir, downloadsDirName)
}

// driverDownloadsDir is the reserved seed's download dir under dataDir: the cwd
// `cuttle pw` execs the bundled driver in, whatever seed the browser runs under
// (pool mode's default seed, an --ephemeral profile), so it is where the
// driver's own output lands.
func driverDownloadsDir(dataDir string) string {
	return filepath.Join(dataDir, reservedSeed, downloadsDirName)
}

// handleDownloadsList returns the seed's completed downloads, newest first.
// Dotfiles and Chrome's in-progress .crdownload partials are omitted, so a
// listed file is safe to pull.
func (m *multiplexer) handleDownloadsList(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	_, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	entries, err := os.ReadDir(downloadsDir(inst))
	if err != nil {
		// No directory yet means no downloads, not an error.
		writeJSON(w, http.StatusOK, map[string]any{"downloads": []any{}})
		return
	}
	type fileEntry struct {
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		Modified string `json:"modified"`
	}
	files := []fileEntry{}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".crdownload") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		files = append(files, fileEntry{
			Name:     name,
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	// RFC3339 UTC timestamps sort lexicographically in chronological order.
	slices.SortFunc(files, func(a, b fileEntry) int { return strings.Compare(b.Modified, a.Modified) })
	writeJSON(w, http.StatusOK, map[string]any{"downloads": files})
}

// handleDownloadsGet streams one downloaded file. The name must be a bare
// filename - a single path component, not dot-prefixed - so `filepath.Join`
// with it can never escape the seed's download dir into the rest of the profile
// (cookies, tokens). The body is always served as an opaque attachment:
// downloads are untrusted content, and rendering one as HTML on this loopback
// origin would hand it the daemon's own API.
func (m *multiplexer) handleDownloadsGet(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	_, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	name := r.PathValue("name")
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "invalid download name"})
		return
	}
	f, err := os.Open(filepath.Join(downloadsDir(inst), name))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{keyError: "no such download"})
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeJSON(w, http.StatusNotFound, map[string]any{keyError: "no such download"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	_, _ = io.Copy(w, f)
}

// snapshotFilePattern is the one shape of path `cuttle pw` may ask for: the page
// snapshot the bundled driver writes after an action verb, relative to its
// working directory - the default seed's download dir. Anchored and free of
// "..", it can never name anything outside the driver's own snapshot dir.
var snapshotFilePattern = regexp.MustCompile(`^\.playwright-cli/page-[0-9TZ-]+\.yml$`)

// passwordPlaceholder stands in for a password field's value. Its name can never
// be a secret's (a hyphen fails secretNamePattern), so it cannot be mistaken for
// a sentinel an agent could type.
const passwordPlaceholder = sentinelPrefix + "password-field" + sentinelSuffix

// passwordReadTimeout bounds the page read behind one /snapshot. `cuttle pw`
// fetches the route with a 2s ceiling, so a slow browser must cost the field
// mask, not the whole snapshot.
const passwordReadTimeout = 1200 * time.Millisecond

// passwordFieldsJS returns the current value of every filled password field in
// the document and its same-origin iframes - the values only, never which field.
// A cross-origin iframe is a separate target and is not walked. The page cannot
// touch this function (it runs in an isolated world), but it does decide how
// many fields there are and how long their values are, so both are capped here,
// before Chrome serializes the answer: the daemon reads the reply with no frame
// size limit, and it must not be a page's choice how big that reply is.
const passwordFieldsJS = `function(){var out=[];var walk=function(d){` +
	`d.querySelectorAll('input[type=password]').forEach(function(i){` +
	`if(i.value&&i.value.length<=4096&&out.length<32)out.push(i.value);});` +
	`d.querySelectorAll('iframe').forEach(function(f){try{if(f.contentDocument)walk(f.contentDocument);}catch(e){}});};` +
	`try{walk(document);}catch(e){}return out;}`

// passwordFieldValues reads the password fields of every open page in the
// seed's browser, since the daemon cannot tell which one the driver snapshotted.
// Best effort by design: a browser that cannot be reached yields nothing, and the
// snapshot is served masked by held values alone - a value the agent typed as a
// literal was already on its side of the boundary.
func passwordFieldValues(ctx context.Context, port int) []string {
	ctx, cancel := context.WithTimeout(ctx, passwordReadTimeout)
	defer cancel()
	var pages []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := fetchCDP(ctx, port, "/json/list", &pages); err != nil {
		return nil
	}
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return nil
	}
	defer conn.close() // closing the socket detaches every session it opened
	var values []string
	for _, p := range pages {
		if p.Type != targetPage || p.ID == "" {
			continue
		}
		sid, err := conn.attach(ctx, p.ID)
		if err != nil {
			continue
		}
		res, err := conn.callInWorld(ctx, sid, passwordFieldsJS, "", true)
		if err != nil {
			continue
		}
		result, _ := res[cdpResult].(map[string]any)
		found, _ := result[cdpValue].([]any)
		for _, v := range found {
			if s, ok := v.(string); ok {
				values = append(values, s)
			}
		}
	}
	return values
}

// handleSnapshot returns one driver snapshot with every value the seed's secret
// store holds replaced by its sentinel, and every password field's current
// value by passwordPlaceholder, so the copy `cuttle pw` leaves on the host
// never carries a credential - whether the agent filled it by sentinel or typed
// it as a literal (the driver renders a password field as a plain textbox with
// its value).
func (m *multiplexer) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	file := r.URL.Query().Get("file")
	if !snapshotFilePattern.MatchString(file) {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "invalid snapshot file"})
		return
	}
	seed, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	// The snapshot is where the driver writes, not in the profile of the seed
	// asked for; the instance only supplies the seed to mask for. os.Root refuses
	// a symlink or ".." that would leave the dir, on top of what the pattern
	// already rules out.
	root, err := os.OpenRoot(driverDownloadsDir(m.pool.dataDir))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{keyError: "no such snapshot"})
		return
	}
	defer func() { _ = root.Close() }()
	body, err := root.ReadFile(file)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{keyError: "no such snapshot"})
		return
	}
	pairs := m.pool.secrets.heldPairs(seed)
	for _, v := range passwordFieldValues(r.Context(), inst.cdpPort) {
		pairs = append(pairs, exactPairs(v, passwordPlaceholder)...)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, maskExact(string(body), pairs)) //nolint:gosec // text/plain + nosniff: never rendered
}
