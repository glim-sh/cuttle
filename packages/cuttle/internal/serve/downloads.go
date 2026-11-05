package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
		if !finishedDownload(e) {
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
	if !bareDownloadName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{keyError: "invalid download name"})
		return
	}
	serveDownload(w, downloadsDir(inst), name)
}

// bareDownloadName holds for a single, not dot-prefixed path component, which
// filepath.Join cannot take out of a download dir.
func bareDownloadName(name string) bool {
	return name == filepath.Base(name) && !strings.HasPrefix(name, ".")
}

// finishedDownload holds for a regular, visible file Chrome is done writing.
func finishedDownload(e os.DirEntry) bool {
	return e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") && !strings.HasSuffix(e.Name(), ".crdownload")
}

// serveDownload streams dir/name as an opaque attachment. name must already be
// a bare filename.
func serveDownload(w http.ResponseWriter, dir, name string) {
	f, err := os.Open(filepath.Join(dir, name))
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
// Pages are read in parallel, each over its own socket, so one slow page cannot
// spend the shared budget and leave the pages after it unread.
// A page that cannot be read - a native dialog blocks every evaluate in it, or it
// is slow or mid-navigation - contributes the values memo last read from it, so
// a failed read never unmasks a field that was masked a moment ago. A browser
// never read yields nothing, and the snapshot is served masked by held values
// alone - a value the agent typed as a literal was already on its side of the
// boundary.
func passwordFieldValues(ctx context.Context, port int, memo *passwordMemo) []string {
	ctx, cancel := context.WithTimeout(ctx, passwordReadTimeout)
	defer cancel()
	gen := memo.begin()
	var pages []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := fetchCDP(ctx, port, "/json/list", &pages); err != nil {
		return memo.all()
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		read = map[string][]string{}
		live = map[string]bool{}
	)
	slots := make(chan struct{}, passwordReadParallel)
	for _, p := range pages {
		if p.Type != targetPage || p.ID == "" {
			continue
		}
		live[p.ID] = true
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			found, ok := pagePasswordValues(ctx, port, p.ID)
			if !ok {
				return
			}
			mu.Lock()
			read[p.ID] = found
			mu.Unlock()
		})
	}
	wg.Wait()
	return memo.update(gen, live, read)
}

// passwordReadParallel bounds the sockets one password read opens at once.
const passwordReadParallel = 8

// passwordMemo is the last password field values read from each page of one
// browser, kept while the page is open. Reads overlap (every principal's
// masking, the snapshot route), so each is numbered when it starts and an
// older read never overwrites what a newer one found.
type passwordMemo struct {
	mu    sync.Mutex
	gen   uint64
	pages map[string]memoRead
}

type memoRead struct {
	gen    uint64
	values []string
}

func (m *passwordMemo) begin() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gen++
	return m.gen
}

// update records what read gen found, keeps the last values of the live pages
// it could not read, forgets the pages it saw closed, and returns every value.
func (m *passwordMemo) update(gen uint64, live map[string]bool, read map[string][]string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := map[string]memoRead{}
	for id, r := range m.pages {
		if live[id] || r.gen > gen {
			next[id] = r
		}
	}
	for id, v := range read {
		if r, ok := next[id]; !ok || r.gen < gen {
			next[id] = memoRead{gen, v}
		}
	}
	m.pages = next
	return m.values()
}

// all is every value last read from the browser.
func (m *passwordMemo) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values()
}

func (m *passwordMemo) values() []string {
	var out []string
	for _, r := range m.pages {
		out = append(out, r.values...)
	}
	return out
}

// pagePasswordValues reads one page's password fields; ok is false when the
// page could not be read, which is not the same as a page with none filled.
func pagePasswordValues(ctx context.Context, port int, targetID string) ([]string, bool) {
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return nil, false
	}
	defer conn.close() // closing the socket detaches the session it opened
	sid, err := conn.attach(ctx, targetID)
	if err != nil {
		return nil, false
	}
	res, err := conn.callInWorld(ctx, sid, passwordFieldsJS, "", true)
	if err != nil {
		return nil, false
	}
	result, _ := res[cdpResult].(map[string]any)
	found, isList := result[cdpValue].([]any)
	if !isList || res["exceptionDetails"] != nil {
		return nil, false
	}
	var values []string
	for _, v := range found {
		if s, ok := v.(string); ok {
			values = append(values, s)
		}
	}
	return values, true
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
	pairs := m.maskPairs(r.Context(), seed, inst)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = io.WriteString(w, maskExact(string(body), pairs)) //nolint:gosec // text/plain + nosniff: never rendered
}

const maskLineLimit = 8 << 20

var errMaskLineLimit = errors.New("mask input line exceeds 8 MiB")

func (m *multiplexer) maskPairs(ctx context.Context, seed string, inst *chromeInstance) [][]string {
	pairs := m.pool.secrets.heldPairs(seed)
	for _, v := range passwordFieldValues(ctx, inst.cdpPort, &inst.passwords) {
		pairs = append(pairs, exactPairs(v, passwordPlaceholder)...)
	}
	encoded := jsonPairs(pairs)
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	for _, pair := range pairs {
		buf.Reset()
		if err := encoder.Encode(pair[0]); err == nil {
			value := buf.String()
			encoded = append(encoded, []string{value[1 : len(value)-2], pair[1]})
		}
	}
	return append(pairs, encoded...)
}

func (m *multiplexer) handleMask(w http.ResponseWriter, r *http.Request) {
	if m.rejectUntrustedLoopback(w, r) {
		return
	}
	seed, inst := m.runningSeedInstance(w, r)
	if inst == nil {
		return
	}
	controller := http.NewResponseController(w)
	if err := controller.EnableFullDuplex(); err != nil {
		http.Error(w, "streaming masking unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := controller.Flush(); err != nil {
		return
	}
	if err := maskStream(w, r.Body, func() [][]string { return m.maskPairs(r.Context(), seed, inst) }); err != nil {
		// A truncated successful response must not look like a completed mask operation.
		panic(http.ErrAbortHandler)
	}
}

func maskStream(w io.Writer, r io.Reader, readPairs func() [][]string) error {
	var pairs [][]string
	buf := make([]byte, 32<<10)
	pending := ""
	clean := 0 // pending[:clean] holds no newline
	lineSize := 0
	for {
		n, err := r.Read(buf)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read mask input: %w", err)
		}
		for _, b := range buf[:n] {
			if b == '\n' {
				lineSize = 0
			} else {
				lineSize++
			}
			if lineSize > maskLineLimit {
				return errMaskLineLimit
			}
		}
		if n > 0 {
			fresh := readPairs()
			// A longer partial match can fail later, exposing shorter matches in its tail.
			for _, pair := range pairs {
				keep := strings.Contains(pending, pair[0])
				for start := max(0, len(pending)-len(pair[0])+1); !keep && start < len(pending); start++ {
					keep = strings.HasPrefix(pair[0], pending[start:])
				}
				if pending != "" && keep && !slices.ContainsFunc(fresh, func(p []string) bool { return p[0] == pair[0] }) {
					fresh = append(fresh, pair)
				}
			}
			pairs = fresh
			slices.SortStableFunc(pairs, func(a, b []string) int { return len(b[0]) - len(a[0]) })
		}
		pending += string(buf[:n])
		end, cut := len(pending), len(pending)
		if !errors.Is(err, io.EOF) {
			longest := 0
			for _, pair := range pairs {
				longest = max(longest, len(pair[0]))
			}
			// Release only through a newline followed by longest-1 more bytes (or at EOF), so the
			// release point never depends on whether pending bytes start a held value.
			end = min(len(pending), max(0, len(pending)-longest+1))
			clean = min(clean, end)
			cut = 0
			if i := strings.LastIndexByte(pending[clean:end], '\n'); i >= 0 {
				cut = clean + i + 1
			}
		}
		var out strings.Builder
		pos := 0
	scan:
		for pos < cut {
			for _, pair := range pairs {
				if pair[0] != "" && strings.HasPrefix(pending[pos:], pair[0]) {
					out.WriteString(pair[1])
					pos += len(pair[0])
					continue scan
				}
			}
			out.WriteByte(pending[pos])
			pos++
		}
		if out.Len() > 0 {
			if _, writeErr := io.WriteString(w, out.String()); writeErr != nil {
				return fmt.Errorf("write masked output: %w", writeErr)
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		pending = pending[pos:]
		clean = max(0, end-pos)
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
