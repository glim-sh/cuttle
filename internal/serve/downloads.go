package serve

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// downloadsInstance resolves the request's seed (?fingerprint=, defaulting like
// a connect URL) to its running Chrome. Serving requires a live instance: the
// download flow is click-then-pull against the running session, and only a live
// instance pins which profile dir (durable or ephemeral scratch) is current.
// Writes the error response and returns nil when there is nothing to serve.
func (m *multiplexer) downloadsInstance(w http.ResponseWriter, r *http.Request) *chromeInstance {
	seedKey, ok := m.pool.seedKeyFor(r.URL.Query().Get(keyFingerprint))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: msgInvalidSeed})
		return nil
	}
	inst := m.pool.runningInstance(seedKey)
	if inst == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{keyError: "seed not running"})
	}
	return inst
}

// handleDownloadsList returns the seed's completed downloads, newest first.
// Dotfiles and Chrome's in-progress .crdownload partials are omitted, so a
// listed file is safe to pull.
func (m *multiplexer) handleDownloadsList(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	inst := m.downloadsInstance(w, r)
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
	inst := m.downloadsInstance(w, r)
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
