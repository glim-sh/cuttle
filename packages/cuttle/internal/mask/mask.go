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

// Credential is one provider-designed credential prefix: a token shape whose
// issuer made it recognizable on purpose. Nothing here guesses at entropy -
// every rule is a published prefix plus that issuer's length and charset.
type Credential struct {
	Name    string
	Pattern *regexp.Regexp
	// Valid, when set, rejects a match the pattern alone cannot.
	Valid func(match string) bool
}

// Credentials is the whole auto-capture table - one slice, edited here and
// nowhere else. Adding a shape is one line; the surrounding code needs no
// change.
//
// Deliberately NOT here: any generic high-entropy rule, and anything personal
// (email, card, IBAN, phone). Those are data an agent legitimately reads, and a
// false positive replaces a piece of the page with a placeholder it then has to
// reason around.
var Credentials = []Credential{
	{Name: "github-fine-grained-pat", Pattern: regexp.MustCompile(`github_pat_[A-Za-z0-9_]{82}`)},
	{Name: "github-pat", Pattern: regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)},
	// One rule for every "secret key" prefix in the wild: OpenAI `sk-`, Anthropic
	// `sk-ant-`, OpenRouter `sk-or-`, Stripe `sk_live_` and its restricted
	// sibling `rk_live_`. The 20-character floor and the token boundary are what
	// keep it off ordinary kebab-case text ("sk-limit", "task_id").
	{Name: "api-secret-key", Pattern: regexp.MustCompile(`[sr]k[-_][A-Za-z0-9_-]{20,}`)},
	{Name: "slack-token", Pattern: regexp.MustCompile(`xox[abprs]-[A-Za-z0-9-]{10,}`), Valid: hasDigit},
	{Name: "aws-access-key-id", Pattern: regexp.MustCompile(`AKIA[A-Z0-9]{16}`)},
	{Name: "gitlab-pat", Pattern: regexp.MustCompile(`glpat-[A-Za-z0-9_-]{20}`)},
	{Name: "age-secret-key", Pattern: regexp.MustCompile(`AGE-SECRET-KEY-1[A-Z0-9]{58}`)},
	// Three base64url segments whose header really is a JOSE header: "eyJ" alone
	// is just `{"` in base64 and turns up in plenty of harmless payloads.
	{Name: "jwt", Pattern: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), Valid: joseHeader},
	// A snapshot quotes page text, so the line breaks can arrive as literal `\n`.
	{Name: "private-key", Pattern: regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`)},
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
		for _, loc := range c.Pattern.FindAllStringIndex(text, -1) {
			start, end := loc[0], loc[1]
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
