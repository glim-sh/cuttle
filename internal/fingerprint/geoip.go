package fingerprint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	geoip2 "github.com/oschwald/geoip2-golang/v2"
	"golang.org/x/net/proxy"
)

// CountryLocaleMap maps an ISO 3166-1 alpha-2 country code to a BCP 47 locale.
// Pinned by the golden snapshot; a change must be a reviewed golden diff.
var CountryLocaleMap = map[string]string{
	"US": "en-US", "GB": "en-GB", "AU": "en-AU", "CA": "en-CA", "NZ": "en-NZ",
	"IE": "en-IE", "ZA": "en-ZA", "SG": "en-SG",
	"DE": "de-DE", "AT": "de-AT", "CH": "de-CH",
	"FR": "fr-FR", "BE": "fr-BE",
	"ES": "es-ES", "MX": "es-MX", "AR": "es-AR", "CO": "es-CO", "CL": "es-CL",
	"BR": "pt-BR", "PT": "pt-PT",
	"IT": "it-IT", "NL": "nl-NL",
	"JP": "ja-JP", "KR": "ko-KR", "CN": "zh-CN", "TW": "zh-TW", "HK": "zh-HK",
	"RU": "ru-RU", "UA": "uk-UA", "PL": "pl-PL", "CZ": "cs-CZ", "RO": "ro-RO",
	"IL": "he-IL", "TR": "tr-TR", "SA": "ar-SA", "AE": "ar-AE", "EG": "ar-EG",
	"IN": "hi-IN", "ID": "id-ID", "PH": "en-PH",
	"TH": "th-TH", "VN": "vi-VN", "MY": "ms-MY",
	"SE": "sv-SE", "NO": "nb-NO", "DK": "da-DK", "FI": "fi-FI",
	"GR": "el-GR", "HU": "hu-HU", "BG": "bg-BG",
	"SI": "sl-SI", "SK": "sk-SK", "HR": "hr-HR", "RS": "sr-RS", "LT": "lt-LT",
	"LV": "lv-LV", "EE": "et-EE", "IS": "is-IS", "LU": "fr-LU", "MT": "en-MT",
	"CY": "el-CY", "MD": "ro-MD", "BY": "ru-BY", "GE": "ka-GE", "AL": "sq-AL",
	"MK": "mk-MK", "BA": "bs-BA",
	"PE": "es-PE", "VE": "es-VE", "EC": "es-EC", "UY": "es-UY", "CR": "es-CR",
	"DO": "es-DO", "GT": "es-GT", "BO": "es-BO", "PY": "es-PY",
	"PK": "en-PK", "BD": "bn-BD", "LK": "si-LK", "KZ": "ru-KZ", "IR": "fa-IR",
	"IQ": "ar-IQ", "JO": "ar-JO", "LB": "ar-LB", "KW": "ar-KW", "QA": "ar-QA",
	"OM": "ar-OM", "BH": "ar-BH",
	"NG": "en-NG", "KE": "en-KE", "MA": "fr-MA", "DZ": "ar-DZ", "TN": "ar-TN",
	"GH": "en-GH",
	"AM": "hy-AM", "AZ": "az-AZ", "UZ": "uz-UZ", "KG": "ky-KG", "TJ": "tg-TJ",
	"TM": "tk-TM",
	"ME": "sr-ME", "XK": "sq-XK", "LI": "de-LI", "MC": "fr-MC", "AD": "ca-AD",
	"MM": "my-MM", "KH": "km-KH", "LA": "lo-LA", "MN": "mn-MN", "BN": "ms-BN",
	"MO": "zh-MO",
	"YE": "ar-YE", "SY": "ar-SY", "PS": "ar-PS", "LY": "ar-LY",
	"ET": "am-ET", "TZ": "sw-TZ", "UG": "en-UG", "SN": "fr-SN", "CI": "fr-CI",
	"CM": "fr-CM", "AO": "pt-AO", "MZ": "pt-MZ", "ZM": "en-ZM", "ZW": "en-ZW",
	"HN": "es-HN", "NI": "es-NI", "SV": "es-SV", "PA": "es-PA", "JM": "en-JM",
	"TT": "en-TT", "PR": "es-PR",
}

const (
	geoIPDBURL          = "https://github.com/P3TERX/GeoLite.mmdb/raw/download/GeoLite2-City.mmdb"
	geoIPDBFilename     = "GeoLite2-City.mmdb"
	geoIPUpdateInterval = 30 * 24 * time.Hour
	echoTimeout         = 10 * time.Second
)

var (
	errNoExitIP        = errors.New("failed to discover exit IP")
	errGeoIPDownStatus = errors.New("geoip download: unexpected status")
)

// ipEchoURLs are public IP-echo services queried (through the proxy when set) to
// discover the egress IP.
var ipEchoURLs = []string{
	"https://api.ipify.org",
	"https://checkip.amazonaws.com",
	"https://ifconfig.me/ip",
}

// ExitIPFunc resolves the egress IP for a proxy URL ("" = the machine's own
// public IP). Injected so callers can stub network access in tests.
type ExitIPFunc func(proxyURL string) (string, error)

// GeoResolver resolves timezone/locale/exit-IP from a proxy. All fields are
// injectable for hermetic testing; the zero value is not usable - construct via
// [NewGeoResolver].
type GeoResolver struct {
	ExitIP ExitIPFunc
	DBPath func() string
	// ResolveHost DNS-resolves the proxy's own hostname to an IP. It is the
	// fallback egress IP when every echo service is unreachable but the proxy
	// host still resolves (common for datacenter proxies that block outbound to
	// the echo endpoints).
	ResolveHost func(proxyURL string) string
}

// NewGeoResolver returns a resolver wired to the real echo services, the cached
// mmdb (downloaded on first use), and DNS-based proxy-host fallback.
func NewGeoResolver() GeoResolver {
	return GeoResolver{ExitIP: DefaultExitIP, DBPath: ensureGeoIPDB, ResolveHost: resolveProxyHostIP}
}

// ResolveProxyGeoWithIP returns (timezone, locale, exitIP). When the echo
// services fail, it falls back to DNS-resolving the proxy hostname (gateway geo)
// rather than dropping the IP, so WebRTC never leaks the real address behind a
// proxy. A missing or failed mmdb still returns the exit IP; any lookup failure
// degrades gracefully rather than erroring.
func (r GeoResolver) ResolveProxyGeoWithIP(proxyURL string) (string, string, string) {
	if r.ExitIP == nil {
		return "", "", ""
	}
	ip, err := r.ExitIP(proxyURL)
	if (err != nil || ip == "") && proxyURL != "" && r.ResolveHost != nil {
		ip = r.ResolveHost(proxyURL)
	}
	if ip == "" {
		return "", "", ""
	}
	dbPath := ""
	if r.DBPath != nil {
		dbPath = r.DBPath()
	}
	if dbPath == "" {
		return "", "", ip
	}
	db, err := geoip2.Open(dbPath)
	if err != nil {
		return "", "", ip
	}
	defer func() { _ = db.Close() }()

	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "", "", ip
	}
	city, err := db.City(addr)
	if err != nil {
		return "", "", ip
	}
	loc := ""
	if city.Country.ISOCode != "" {
		loc = CountryLocaleMap[city.Country.ISOCode]
	}
	return city.Location.TimeZone, loc, ip
}

// DefaultExitIP discovers the egress IP by querying the echo services through
// the proxy (or directly when proxyURL is empty).
func DefaultExitIP(proxyURL string) (string, error) {
	client, err := echoClient(proxyURL)
	if err != nil {
		return "", err
	}
	for _, u := range ipEchoURLs {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		_ = resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(body))
		if _, err := netip.ParseAddr(ip); err != nil {
			continue
		}
		return ip, nil
	}
	return "", errNoExitIP
}

// resolveProxyHostIP extracts the proxy hostname and resolves it to an IP: a
// literal IP is returned as-is, otherwise the first DNS result. Returns "" when
// the host is absent or unresolvable.
func resolveProxyHostIP(proxyURL string) string {
	host := urlsplit(proxyURL).hostname()
	if host == "" {
		return ""
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String()
	}
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil || len(ips) == 0 {
		return ""
	}
	return ips[0].IP.String()
}

func echoClient(proxyURL string) (*http.Client, error) {
	if proxyURL == "" {
		return &http.Client{Timeout: echoTimeout}, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		dialer, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, err //nolint:wrapcheck
		}
		tr := &http.Transport{}
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			tr.DialContext = cd.DialContext
		}
		return &http.Client{Transport: tr, Timeout: echoTimeout}, nil
	default:
		return &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyURL(u)},
			Timeout:   echoTimeout,
		}, nil
	}
}

func geoIPDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err //nolint:wrapcheck
	}
	return filepath.Join(base, "cuttle", "geoip"), nil
}

// ensureGeoIPDB returns the cached mmdb path, downloading it on first use and
// triggering a background refresh once older than the update interval. Returns
// "" when unavailable so geo resolution degrades to exit-IP-only.
func ensureGeoIPDB() string {
	dir, err := geoIPDir()
	if err != nil {
		return ""
	}
	dbPath := filepath.Join(dir, geoIPDBFilename)
	if info, err := os.Stat(dbPath); err == nil {
		if time.Since(info.ModTime()) >= geoIPUpdateInterval {
			go func() { _ = downloadGeoIPDB(dbPath) }()
		}
		return dbPath
	}
	if err := downloadGeoIPDB(dbPath); err != nil {
		return ""
	}
	return dbPath
}

func downloadGeoIPDB(dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err //nolint:wrapcheck
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geoIPDBURL, nil)
	if err != nil {
		return err //nolint:wrapcheck
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err //nolint:wrapcheck
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %s", errGeoIPDownStatus, resp.Status)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "*.tmp")
	if err != nil {
		return err //nolint:wrapcheck
	}
	tmpName := tmp.Name()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err //nolint:wrapcheck
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err //nolint:wrapcheck
	}
	return os.Rename(tmpName, dest) //nolint:wrapcheck
}
