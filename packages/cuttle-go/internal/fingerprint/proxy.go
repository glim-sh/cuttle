package fingerprint

import (
	"slices"
	"strconv"
	"strings"
)

// ProxySettings mirrors the Playwright-compatible proxy dict.
type ProxySettings struct {
	Server   string
	Username string
	Password string
}

// EnsureProxyScheme prepends http:// to a schemeless proxy URL so parsers can
// extract the hostname.
func EnsureProxyScheme(proxyURL string) string {
	if strings.Contains(proxyURL, "://") {
		return proxyURL
	}
	return "http://" + proxyURL
}

// IsSocksProxy reports whether the proxy URL uses the SOCKS5 scheme.
func IsSocksProxy(url string) bool {
	l := strings.ToLower(url)
	return strings.HasPrefix(l, "socks5://") || strings.HasPrefix(l, "socks5h://")
}

// ExtractProxyURL normalizes a proxy dict into a URL string with inline
// credentials, reconstructing SOCKS5 credentials so SOCKS5 auth works.
func ExtractProxyURL(p ProxySettings) string {
	if p.Server == "" {
		return ""
	}
	if IsSocksProxy(p.Server) {
		return ReconstructSocksURL(p)
	}
	return EnsureProxyScheme(p.Server)
}

func extractProxyURLString(proxy string) string {
	if proxy == "" {
		return ""
	}
	return EnsureProxyScheme(proxy)
}

// ReconstructSocksURL rebuilds a SOCKS5 URL with inline credentials from a proxy
// dict. With no username, the server string is returned unchanged.
func ReconstructSocksURL(p ProxySettings) string {
	if p.Username == "" {
		return p.Server
	}
	u := urlparse(p.Server)
	encUser := pyQuote(p.Username)
	encPass := ""
	passPresent := false
	if p.Password != "" {
		encPass = pyQuote(p.Password)
		passPresent = true
	}
	port, hasPort, _ := u.port()
	return assembleProxyURL(u.scheme, u.hostname(), hasPort, port, encUser, encPass, passPresent, u.path, "", "", "")
}

// NormalizeSocksStringURL re-encodes credentials in a proxy URL so Chromium's
// parser does not truncate them at special characters. It is idempotent on
// already-encoded input and passes unparseable input through unchanged.
func NormalizeSocksStringURL(rawurl string) string {
	u := urlparse(rawurl)
	if _, _, err := u.port(); err != nil {
		return rawurl
	}
	user, userSet, pass, passSet := u.userinfo()
	if !userSet && !passSet {
		return rawurl
	}
	rawUser := ""
	if userSet {
		rawUser = user
	}
	encUser := ""
	if rawUser != "" {
		encUser = pyQuote(pyUnquote(rawUser))
	}
	encPass := ""
	passPresent := false
	if passSet {
		passPresent = true
		if pass != "" {
			encPass = pyQuote(pyUnquote(pass))
		}
	}
	port, hasPort, _ := u.port()
	return assembleProxyURL(u.scheme, u.hostname(), hasPort, port, encUser, encPass, passPresent, u.path, u.params, u.query, u.fragment)
}

// SplitProxyAuth strips inline credentials from an http(s) proxy URL. Stock
// Chromium and the free forks reject credentials on --proxy-server, so the
// cred-less URL is used there and the username/password are answered over CDP
// (Fetch.continueWithAuth). SOCKS and cred-less proxies pass through unchanged
// with empty credentials.
//
// It byte-matches CPython's urlsplit + SplitResult.username/password (raw,
// percent-encoding preserved) / hostname (lowercased) / port (re-rendered) +
// urlunsplit, so both the argv and the credentials answered to the proxy are
// identical to the Python oracle.
func SplitProxyAuth(proxy string) (string, string, string) {
	u := urlsplit(proxy)
	rawUser, userSet, rawPass, _ := u.userinfo()
	if (u.scheme != "http" && u.scheme != "https") || !userSet {
		return proxy, "", ""
	}
	host := u.hostname()
	if p, ok, err := u.port(); err == nil && ok && p != 0 {
		host = host + ":" + strconv.Itoa(p)
	}
	stripped := urlunsplit(u.scheme, host, u.path, u.query, u.fragment)
	return stripped, rawUser, rawPass
}

// ResolveWebRTCArgs replaces --fingerprint-webrtc-ip=auto with the resolved
// proxy exit IP. With no proxy or an unresolvable exit IP, the flag is dropped.
// resolveExitIP is injected so callers can stub network access in tests.
func ResolveWebRTCArgs(args []string, proxy string, resolveExitIP func(proxyURL string) string) []string {
	if len(args) == 0 {
		return args
	}
	idx := slices.Index(args, "--fingerprint-webrtc-ip=auto")
	if idx == -1 {
		return args
	}
	out := slices.Clone(args)
	proxyURL := extractProxyURLString(proxy)
	if proxyURL == "" {
		return slices.Delete(out, idx, idx+1)
	}
	if exitIP := resolveExitIP(proxyURL); exitIP != "" {
		out[idx] = "--fingerprint-webrtc-ip=" + exitIP
		return out
	}
	return slices.Delete(out, idx, idx+1)
}

// assembleProxyURL builds a proxy URL from percent-encoded credentials and host
// parts. passPresent distinguishes user@host (false) from user:@host (true,
// empty password preserves the colon).
func assembleProxyURL(scheme, host string, hasPort bool, port int, encUser, encPass string, passPresent bool, path, params, query, fragment string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	userinfo := ""
	switch {
	case passPresent:
		userinfo = encUser + ":" + encPass + "@"
	case encUser != "":
		userinfo = encUser + "@"
	}
	netloc := userinfo + host
	if hasPort {
		netloc += ":" + strconv.Itoa(port)
	}
	return urlunparse(scheme, netloc, path, params, query, fragment)
}
