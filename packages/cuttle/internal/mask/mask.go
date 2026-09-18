// Package mask scrubs credential-shaped text that cuttle is about to print or
// log. It is shared because the daemon and the CLI both emit URLs the browser
// has been to, and a rule this specific drifts the moment it exists twice: the
// leak it was written for was a routine retry log carrying
// `remix_userkey=25039df9...` in the URL it was retrying, and an OAuth callback
// an agent lands on carries the same shape in `?code=`.
package mask

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// credentialParamRE matches a credential-shaped query or fragment parameter. It
// needs no knowledge of any stored value, which is the point: the values it
// catches are ones nothing ever held.
var credentialParamRE = regexp.MustCompile(
	`(?i)([A-Za-z0-9_.-]*(?:token|key|secret|password|passwd|pwd|auth|session|credential|code)[A-Za-z0-9_.-]*)=([A-Za-z0-9%._~+/-]{8,}=*)`,
)

// Params replaces the value of every credential-shaped parameter with a
// placeholder, leaving ordinary ones alone.
func Params(text string) string {
	// The regex forces the NFA engine at every position and cannot match without
	// an `=`, so one IndexByte keeps the common line off that path entirely.
	if strings.IndexByte(text, '=') < 0 {
		return text
	}
	return credentialParamRE.ReplaceAllString(text, "$1=<redacted>")
}

// Credential is one recognizable credential shape: a prefix its issuer made
// recognizable on purpose, or one of two structures (a secret-labelled form
// field, a password inline in a URL) that name themselves. Nothing here guesses
// at entropy.
type Credential struct {
	Name    string
	Pattern *regexp.Regexp
	// Group, when set, is the capture group holding the credential; the rest of
	// the match is context that stays readable.
	Group int
	// Valid, when set, rejects a match the pattern alone cannot.
	Valid func(match string) bool
}

// re keeps the table one line per rule.
func re(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// Credentials is the whole auto-capture table - one slice, edited here and
// nowhere else. Adding a shape is one line; the surrounding code needs no
// change.
//
// Deliberately NOT here: any generic high-entropy rule, anything personal
// (email, card, IBAN, phone), and keys that are public by design (a PostHog
// `phc_` project key). A false positive replaces a piece of the page with a
// placeholder the agent then has to reason around.
var Credentials = []Credential{
	{Name: "github-fine-grained-pat", Pattern: re(`github_pat_[A-Za-z0-9_]{82}`)},
	{Name: "github-token", Pattern: re(`gh[pousr]_[A-Za-z0-9]{36}`)},
	// One rule for every "secret key" prefix in the wild: OpenAI `sk-`, Anthropic
	// `sk-ant-`, OpenRouter `sk-or-`, Stripe `sk_live_`/`rk_live_`/`rk_test_`.
	// The 20-character floor, the token boundary and the digit are what keep it
	// off ordinary kebab-case text ("sk-limit", "task_id", "sk-learn-model-...").
	{Name: "api-secret-key", Pattern: re(`[sr]k[-_][A-Za-z0-9_-]{20,}`), Valid: hasDigit},
	{Name: "slack-token", Pattern: re(`xox[abeprs]-[A-Za-z0-9-]{10,}`), Valid: hasDigit},
	{Name: "slack-app-token", Pattern: re(`xapp-[A-Za-z0-9-]{10,}`), Valid: hasDigit},
	{Name: "aws-access-key-id", Pattern: re(`A[KS]IA[A-Z0-9]{16}`), Valid: hasDigit},
	{Name: "google-api-key", Pattern: re(`AIza[\w-]{35}`)},
	{Name: "google-oauth-secret", Pattern: re(`GOCSPX-[\w-]{28}`)},
	{Name: "google-access-token", Pattern: re(`ya29\.[\w-]{50,}`)},
	{Name: "gitlab-pat", Pattern: re(`glpat-[A-Za-z0-9_-]{20}`)},
	{Name: "huggingface-token", Pattern: re(`hf_[A-Za-z0-9]{34}`)},
	{Name: "cloudflare-token", Pattern: re(`cf[au]t_[A-Za-z0-9]{40,}`)},
	{Name: "apify-token", Pattern: re(`apify_api_[A-Za-z0-9]{36}`)},
	{Name: "posthog-personal-key", Pattern: re(`phx_[A-Za-z0-9]{40,}`)},
	{Name: "grafana-service-account", Pattern: re(`glsa_[A-Za-z0-9]{32}_[a-f0-9]{8}`)},
	{Name: "linear-key", Pattern: re(`lin_api_[A-Za-z0-9]{40}`)},
	{Name: "npm-token", Pattern: re(`npm_[A-Za-z0-9]{36}`)},
	{Name: "pypi-token", Pattern: re(`pypi-AgEIcHlwaS5vcmc[\w-]{50,}`)},
	{Name: "tailscale-key", Pattern: re(`tskey-(?:auth|api|client)-[A-Za-z0-9]{6,}-[A-Za-z0-9]{16,}`)},
	{Name: "sentry-token", Pattern: re(`sntrys_[\w+/=]{60,}`)},
	{Name: "telegram-bot-token", Pattern: re(`\d{8,10}:AA[\w-]{33}`)},
	{Name: "age-secret-key", Pattern: re(`AGE-SECRET-KEY-1[A-Z0-9]{58}`)},
	// Three base64url segments whose header really is a JOSE header: "eyJ" alone
	// is just `{"` in base64 and turns up in plenty of harmless payloads.
	{Name: "jwt", Pattern: re(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Valid: joseHeader},
	// A snapshot quotes page text, so the line breaks can arrive as literal `\n`.
	{Name: "private-key", Pattern: re(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`)},
	// The inline credential of a URL (`scheme://user:<it>@host`) - only that part; the scheme,
	// user and host stay readable. Docs are full of `postgres://user:password@...`,
	// so a letters-only or templated value is taken for a placeholder.
	{
		Name:    "url-password",
		Pattern: re(`[a-z][a-z0-9+.-]*://[^:/?#\s@]+:([^@\s/?#]{6,})@`),
		Group:   1, Valid: notPlaceholder,
	},
	// A snapshot line for a form field whose label says it holds a secret, e.g.
	// `textbox "API key" [ref=e4]: <value>`. The 16-character floor and the digit
	// skip what such a label usually shows otherwise: a token NAME
	// ("production-deploy-pipeline"), or a masked placeholder.
	{
		Name:    "secret-labelled-field",
		Pattern: re(`textbox "[^"\n]*(?i:secret|token|password|api[ _-]?key)[^"\n]*"(?: \[[^\]\n]*\])*: (\S{16,})`),
		Group:   1, Valid: func(s string) bool { return hasDigit(s) && notOneRepeatedRune(s) },
	},
}

// FindCredentials returns every distinct credential-shaped substring of text, in
// table order. A match glued to more token characters on either side is part of
// something longer - a base64 blob, a hash, a hyphenated word - and is skipped,
// as is one overlapping a match an earlier (more specific) rule already made.
func FindCredentials(text string) []string {
	type span struct{ start, end int }
	var taken []span
	var found []string
	for _, c := range Credentials {
		for _, loc := range c.Pattern.FindAllStringSubmatchIndex(text, -1) {
			start, end := loc[2*c.Group], loc[2*c.Group+1]
			if (start > 0 && tokenByte(text[start-1])) || (end < len(text) && tokenByte(text[end])) {
				continue
			}
			if slices.ContainsFunc(taken, func(s span) bool { return start < s.end && s.start < end }) {
				continue
			}
			match := text[start:end]
			if c.Valid != nil && !c.Valid(match) {
				continue
			}
			taken = append(taken, span{start, end})
			if !slices.Contains(found, match) {
				found = append(found, match)
			}
		}
	}
	return found
}

// tokenByte is a character that continues a token: both base64 alphabets, plus
// the separators these prefixes use. A dot and an `=` are deliberately absent -
// a token ending a sentence, or sitting in a query string, is still a token.
func tokenByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' ||
		b == '_' || b == '-' || b == '+' || b == '/'
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

// notOneRepeatedRune rejects a placeholder such as a row of mask dots.
func notOneRepeatedRune(s string) bool {
	for _, r := range s {
		return strings.Trim(s, string(r)) != ""
	}
	return false
}

// notPlaceholder rejects what documentation puts where a password goes: a word
// ("password", "changeme"), a template ("${DB_PASSWORD}", "<pass>"), a mask.
func notPlaceholder(s string) bool {
	return notOneRepeatedRune(s) && !strings.ContainsAny(s[:1], "$<{[") &&
		strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) })
}

// joseHeader reports whether the first segment really decodes to a JWT header.
func joseHeader(token string) bool {
	header, _, _ := strings.Cut(token, ".")
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(header, "="))
	if err != nil {
		return false
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	_, ok := fields["alg"]
	return ok
}
