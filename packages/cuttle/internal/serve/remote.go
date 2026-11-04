package serve

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	errRemoteMode               = errors.New("remote access requires session mode")
	errRemotePublicURL          = errors.New("public-url must be an HTTPS origin")
	errRemotePrincipalsRequired = errors.New("remote access requires principals-file")
	errRemoteViewerURL          = errors.New("viewer-upstream must be a loopback HTTP origin")
	errPrincipalFormat          = errors.New("invalid principals file: expected name sha256:hex")
	errPrincipalHash            = errors.New("invalid principal hash")
	errPrincipalDuplicate       = errors.New("duplicate principal name or hash")
)

type remoteConfig struct {
	listen, publicURL, principalsFile, viewerUpstream string
}

func (c remoteConfig) validate(mode serveMode) error {
	if c.listen == "" {
		return nil
	}
	if mode != modeSession {
		return errRemoteMode
	}
	u, err := url.Parse(c.publicURL)
	if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errRemotePublicURL
	}
	if c.principalsFile == "" {
		return errRemotePrincipalsRequired
	}
	v, err := url.Parse(c.viewerUpstream)
	if err != nil || v.Scheme != schemeHTTP || !isLoopbackHost(v.Hostname()) || v.User != nil || v.RawQuery != "" || v.Fragment != "" || (v.Path != "" && v.Path != "/") {
		return errRemoteViewerURL
	}
	return nil
}

type remotePrincipal struct {
	name string
	hash [sha256.Size]byte
}

type principalTable struct {
	mu      sync.RWMutex
	entries []remotePrincipal
}

type principalContextKey struct{}

func readPrincipals(file string) ([]remotePrincipal, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("read principals: %w", err)
	}
	defer func() { _ = f.Close() }()
	var entries []remotePrincipal
	names := map[string]bool{}
	hashes := map[[sha256.Size]byte]bool{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validSecretName(fields[0]) || !strings.HasPrefix(fields[1], "sha256:") {
			return nil, errPrincipalFormat
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(fields[1], "sha256:"))
		if err != nil || len(raw) != sha256.Size {
			return nil, errPrincipalHash
		}
		hash := [sha256.Size]byte(raw)
		if names[fields[0]] || hashes[hash] {
			return nil, errPrincipalDuplicate
		}
		names[fields[0]], hashes[hash] = true, true
		entries = append(entries, remotePrincipal{fields[0], hash})
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("read principals: %w", err)
	}
	return entries, nil
}

func (p *principalTable) reload(file string) error {
	entries, err := readPrincipals(file)
	// A missing or invalid replacement must not keep a revoked bearer alive.
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
	return err
}

func (p *principalTable) watch(ctx context.Context, file string) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		case <-tick.C:
		}
		if err := p.reload(file); err != nil {
			logWarn("remote: principals reload failed")
		}
	}
}

func (p *principalTable) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := r.Header.Values("Authorization")
		token := ""
		if len(headers) == 1 && strings.HasPrefix(headers[0], "Bearer ") {
			token = strings.TrimPrefix(headers[0], "Bearer ")
		}
		sum := sha256.Sum256([]byte(token))
		name := ""
		p.mu.RLock()
		for _, entry := range p.entries {
			if subtle.ConstantTimeCompare(sum[:], entry.hash[:]) == 1 {
				name = entry.name
			}
		}
		p.mu.RUnlock()
		if token == "" || strings.ContainsAny(token, " \t\r\n") || name == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, name)))
	})
}

func (m *multiplexer) remoteServer(cfg remoteConfig, machine http.Handler) (*http.Server, *principalTable, error) {
	principals := &principalTable{}
	if err := principals.reload(cfg.principalsFile); err != nil {
		return nil, nil, fmt.Errorf("principals-file: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", m.handleHealthz)
	mux.HandleFunc("GET /readyz", m.handleReadyz)
	mux.Handle("/mcp", principals.authenticate(machine))
	mux.Handle("/files/", principals.authenticate(machine))
	return &http.Server{
		Addr: cfg.listen, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, MaxHeaderValueCount: 100,
	}, principals, nil
}
