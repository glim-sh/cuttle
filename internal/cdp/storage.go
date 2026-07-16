// Package cdp drives an already-running stealth browser over the Chrome DevTools
// Protocol to extract and inject browser storage state. It connects through the
// serve multiplexer's ?fingerprint=<seed> routing via chromedp's RemoteAllocator
// and never launches Chrome itself.
package cdp

import (
	"slices"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
)

// StorageState is the subset of the Playwright storageState JSON shape cuttle
// checks out and back in: global cookies plus per-origin localStorage. It is
// small (auth state, not full Chrome-profile fidelity) so it round-trips over
// CDP between the local canonical copy and an ephemeral remote seed.
type StorageState struct {
	Cookies []Cookie `json:"cookies"`
	Origins []Origin `json:"origins"`
}

// Cookie mirrors a Playwright storageState cookie. Expires is seconds since the
// epoch, or -1 for a session cookie.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite,omitempty"`
}

// Origin is one origin's localStorage in the Playwright storageState shape.
type Origin struct {
	Origin       string             `json:"origin"`
	LocalStorage []LocalStorageItem `json:"localStorage"`
}

// LocalStorageItem is a single localStorage key/value pair.
type LocalStorageItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// OriginURLs returns the origin strings the state carries, so a checkin knows
// which origins to re-read localStorage from.
func (s *StorageState) OriginURLs() []string {
	out := make([]string, 0, len(s.Origins))
	for _, o := range s.Origins {
		out = append(out, o.Origin)
	}
	return out
}

func fromCDPCookies(cs []*network.Cookie) []Cookie {
	out := make([]Cookie, 0, len(cs))
	for _, c := range cs {
		if c == nil {
			continue
		}
		out = append(out, Cookie{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Expires:  c.Expires,
			HTTPOnly: c.HTTPOnly,
			Secure:   c.Secure,
			SameSite: string(c.SameSite),
		})
	}
	return out
}

func toCookieParams(cs []Cookie) []*network.CookieParam {
	out := make([]*network.CookieParam, 0, len(cs))
	for _, c := range cs {
		p := &network.CookieParam{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.Secure,
			HTTPOnly: c.HTTPOnly,
		}
		if c.SameSite != "" {
			p.SameSite = network.CookieSameSite(c.SameSite)
		}
		if c.Expires > 0 {
			sec := int64(c.Expires)
			nsec := int64((c.Expires - float64(sec)) * float64(time.Second))
			ts := cdp.TimeSinceEpoch(time.Unix(sec, nsec))
			p.Expires = &ts
		}
		out = append(out, p)
	}
	return out
}

func itemsToMap(items []LocalStorageItem) map[string]string {
	m := make(map[string]string, len(items))
	for _, it := range items {
		m[it.Name] = it.Value
	}
	return m
}

func mapToItems(m map[string]string) []LocalStorageItem {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]LocalStorageItem, 0, len(m))
	for _, k := range keys {
		out = append(out, LocalStorageItem{Name: k, Value: m[k]})
	}
	return out
}
