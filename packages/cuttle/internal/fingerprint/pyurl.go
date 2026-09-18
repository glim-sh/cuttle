package fingerprint

import (
	"errors"
	"strconv"
	"strings"
)

// This file reimplements the exact subset of CPython's urllib.parse behaviour
// (urlsplit/urlparse component extraction, urlunparse reconstruction, and
// quote/unquote with safe="") that the vendored proxy helpers depend on. Byte
// parity rides on these matching CPython's urllib.parse precisely, so
// the semantics are copied verbatim rather than delegated to net/url (which
// differs in lowercasing, percent-encoding, and userinfo splitting).

const (
	pyAlwaysSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"
	hexUpper     = "0123456789ABCDEF"
	schemeChars  = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."
)

var errBadPort = errors.New("port out of range or non-numeric")

// pyURL holds the six urlparse components.
type pyURL struct {
	scheme   string
	netloc   string
	path     string
	params   string
	query    string
	fragment string
}

// urlparse mirrors CPython urllib.parse.urlparse: urlsplit plus the trailing
// params split from the last path segment.
func urlparse(rawurl string) pyURL {
	u := urlsplit(rawurl)
	if strings.Contains(u.path, ";") {
		u.path, u.params = splitParams(u.path)
	}
	return u
}

func urlsplit(rawurl string) pyURL {
	var u pyURL
	url := rawurl
	if i := strings.IndexByte(url, ':'); i > 0 && isASCIILetter(url[0]) && allSchemeChars(url[1:i]) {
		u.scheme = strings.ToLower(url[:i])
		url = url[i+1:]
	}
	if strings.HasPrefix(url, "//") {
		delim := len(url)
		for _, c := range []byte{'/', '?', '#'} {
			if w := strings.IndexByte(url[2:], c); w >= 0 && w+2 < delim {
				delim = w + 2
			}
		}
		u.netloc = url[2:delim]
		url = url[delim:]
	}
	if i := strings.IndexByte(url, '#'); i >= 0 {
		u.fragment = url[i+1:]
		url = url[:i]
	}
	if i := strings.IndexByte(url, '?'); i >= 0 {
		u.query = url[i+1:]
		url = url[:i]
	}
	u.path = url
	return u
}

func splitParams(path string) (string, string) {
	var i int
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		i = strings.IndexByte(path[slash:], ';')
		if i < 0 {
			return path, ""
		}
		i += slash
	} else {
		i = strings.IndexByte(path, ';')
	}
	return path[:i], path[i+1:]
}

// userinfo returns (username, usernameSet, password, passwordSet), matching
// CPython's rpartition('@') + partition(':') semantics.
func (u pyURL) userinfo() (string, bool, string, bool) {
	at := strings.LastIndexByte(u.netloc, '@')
	if at < 0 {
		return "", false, "", false
	}
	ui := u.netloc[:at]
	if user, pass, found := strings.Cut(ui, ":"); found {
		return user, true, pass, true
	}
	return ui, true, "", false
}

// hostinfo returns the raw host and port strings from netloc.
func (u pyURL) hostinfo() (string, string) {
	hi := u.netloc
	if at := strings.LastIndexByte(hi, '@'); at >= 0 {
		hi = hi[at+1:]
	}
	if _, bracketed, found := strings.Cut(hi, "["); found {
		host, rest, _ := strings.Cut(bracketed, "]")
		_, port, _ := strings.Cut(rest, ":")
		return host, port
	}
	host, port, _ := strings.Cut(hi, ":")
	return host, port
}

// hostname returns the lowercased host, or "" when absent (CPython returns None).
func (u pyURL) hostname() string {
	host, _ := u.hostinfo()
	if host == "" {
		return ""
	}
	return strings.ToLower(host)
}

// port returns (value, present, error); error mirrors CPython raising ValueError
// on a non-numeric or out-of-range port.
func (u pyURL) port() (int, bool, error) {
	_, ps := u.hostinfo()
	if ps == "" {
		return 0, false, nil
	}
	p, err := strconv.Atoi(ps)
	if err != nil {
		return 0, false, errBadPort
	}
	if p < 0 || p > 65535 {
		return 0, false, errBadPort
	}
	return p, true, nil
}

func urlunparse(scheme, netloc, path, params, query, fragment string) string {
	url := path
	if params != "" {
		url = url + ";" + params
	}
	return urlunsplit(scheme, netloc, url, query, fragment)
}

func urlunsplit(scheme, netloc, url, query, fragment string) string {
	if netloc != "" || (scheme != "" && schemeUsesNetloc(scheme) && !strings.HasPrefix(url, "//")) {
		if url != "" && !strings.HasPrefix(url, "/") {
			url = "/" + url
		}
		url = "//" + netloc + url
	}
	if scheme != "" {
		url = scheme + ":" + url
	}
	if query != "" {
		url = url + "?" + query
	}
	if fragment != "" {
		url = url + "#" + fragment
	}
	return url
}

var usesNetloc = map[string]struct{}{
	"": {}, "ftp": {}, "http": {}, "gopher": {}, "nntp": {}, "telnet": {},
	"imap": {}, "wais": {}, "file": {}, "mms": {}, "https": {}, "shttp": {},
	"snews": {}, "prospero": {}, "rtsp": {}, "rtspu": {}, "rsync": {}, "svn": {},
	"svn+ssh": {}, "sftp": {}, "nfs": {}, "git": {}, "git+ssh": {}, "ws": {}, "wss": {},
}

func schemeUsesNetloc(scheme string) bool {
	_, ok := usesNetloc[scheme]
	return ok
}

// pyQuote percent-encodes every byte outside CPython's always-safe set, with
// uppercase hex, matching urllib.parse.quote(s, safe="").
func pyQuote(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		if strings.IndexByte(pyAlwaysSafe, c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexUpper[c>>4])
		b.WriteByte(hexUpper[c&0xf])
	}
	return b.String()
}

// pyUnquote decodes %XX escapes, leaving malformed escapes literal, matching
// urllib.parse.unquote for ASCII input.
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 <= len(s)-1 {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func allSchemeChars(s string) bool {
	for i := range len(s) {
		if strings.IndexByte(schemeChars, s[i]) < 0 {
			return false
		}
	}
	return true
}
