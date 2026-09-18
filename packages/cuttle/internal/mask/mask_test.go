package mask

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParams(t *testing.T) {
	// The kept values are deliberately long enough to match the pattern's own
	// length floor, so surviving proves the KEY was judged, not that the value was
	// too short to be considered.
	cases := map[string]struct{ leaks, keeps string }{
		"retry log":      {"25039df9abc123", "page=1234567890"},
		"oauth callback": {"4/0AVGzR1B7xy", "returnTo=dashboard-overview"},
	}
	lines := map[string]string{
		"retry log":      "retrying https://x.example/api?remix_userkey=25039df9abc123&page=1234567890",
		"oauth callback": "signed in: https://app.example/cb?code=4/0AVGzR1B7xy&returnTo=dashboard-overview",
	}
	for name, tc := range cases {
		got := Params(lines[name])
		if strings.Contains(got, tc.leaks) {
			t.Errorf("%s: the credential survived: %q", name, got)
		}
		if !strings.Contains(got, tc.keeps) {
			t.Errorf("%s: an ordinary parameter was scrubbed: %q", name, got)
		}
	}
	// Nothing to match, nothing to change - including the no-`=` fast path.
	for _, plain := range []string{"a plain log line", "seed=__default__"} {
		if got := Params(plain); got != plain {
			t.Errorf("Params(%q) = %q, want it untouched", plain, got)
		}
	}
}

// Every token here is fabricated: the right shape, none of them a real
// credential. The point of the table is that a provider's own prefix is enough
// to recognize one, so each rule is asserted against exactly that.
func TestCredentialsMatchProviderPrefixes(t *testing.T) {
	// Assembled at runtime so the repo's own secret scan has no literal to flag.
	b64 := base64.RawURLEncoding.EncodeToString
	jwt := b64([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + b64([]byte(`{"sub":"fake"}`)) + "." + b64([]byte("signature-fake"))
	pem := "-----BEGIN RSA PRIVATE KEY-----\nTk9UQVJFQUxLRVk=\nTk9UQVJFQUxLRVk=\n-----END RSA PRIVATE KEY-----"
	tokens := map[string]string{
		"github pat":        "ghp_" + strings.Repeat("A", 36),
		"github app token":  "ghs_" + strings.Repeat("A", 36),
		"github oauth":      "gho_" + strings.Repeat("A", 36),
		"stripe test rk":    "rk_test_" + strings.Repeat("g4", 12),
		"slack user":        "xoxp-1111111111-2222222222-FAKEFAKE",
		"slack app":         "xapp-1-A0FAKE-1111111111-" + strings.Repeat("f", 20),
		"aws session":       "ASIA" + strings.Repeat("Y7", 8),
		"google api":        "AIza" + strings.Repeat("F", 35),
		"google oauth":      "GOCSPX-" + strings.Repeat("F", 28),
		"google access":     "ya29." + strings.Repeat("F", 60),
		"huggingface":       "hf_" + strings.Repeat("F", 34),
		"cloudflare":        "cfut_" + strings.Repeat("F", 40),
		"apify":             "apify_api_" + strings.Repeat("F", 36),
		"posthog personal":  "phx_" + strings.Repeat("F", 40),
		"grafana":           "glsa_" + strings.Repeat("F", 32) + "_" + strings.Repeat("a", 8),
		"linear":            "lin_api_" + strings.Repeat("F", 40),
		"npm":               "npm_" + strings.Repeat("F", 36),
		"pypi":              "pypi-AgEIcHlwaS5vcmc" + strings.Repeat("F", 60),
		"tailscale":         "tskey-auth-" + "kFAKE1CNTRL" + "-" + strings.Repeat("F", 20),
		"sentry":            "sntrys_" + strings.Repeat("F", 70),
		"telegram bot":      "123456789:AA" + strings.Repeat("F", 33),
		"github fine pat":   "github_pat_" + strings.Repeat("B", 82),
		"openai":            "sk-proj-" + strings.Repeat("c3", 20),
		"anthropic":         "sk-ant-api03-" + strings.Repeat("d", 30),
		"openrouter":        "sk-or-v1-" + strings.Repeat("e", 40),
		"stripe live":       "sk_live_" + strings.Repeat("f5", 12),
		"stripe restricted": "rk_live_" + strings.Repeat("g6", 12),
		"slack":             "xoxb-1111111111-2222222222-FAKEFAKEFAKE",
		"aws":               "AKIA" + strings.Repeat("Z9", 8),
		"gitlab":            "glpat-" + strings.Repeat("h", 20),
		"age":               "AGE-SECRET-KEY-1" + strings.Repeat("Q", 58),
		"jwt":               jwt,
		"pem":               pem,
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			found := FindCredentials("the page says: " + token + " (copy it)")
			if len(found) != 1 || found[0] != token {
				t.Fatalf("FindCredentials found %d match(es) %q, want the token itself", len(found), found)
			}
		})
	}
}

// The cost of a false positive is a piece of the page replaced by a placeholder
// the agent then has to reason around, so the ordinary shapes a page is full of
// must not fire.
func TestCredentialsIgnoreOrdinaryPageText(t *testing.T) {
	for name, text := range map[string]string{
		"uuid":            "id 550e8400-e29b-41d4-a716-446655440000",
		"git sha":         "commit 9f2c1e0a4b7d3c5e8f1a2b6d4c7e9f0a1b2c3d4e",
		"base64 image":    "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==",
		"prose":           "the sk- prefix is documented; sk-12 is not a key",
		"short kebab":     "sk-limit reached, see task_id below",
		"embedded prefix": "risk_management_dashboard_widget_column",
		"public key":      "phc_" + strings.Repeat("P", 40),
		"hf in a word":    "shelf_" + strings.Repeat("a", 34),
		"url, no pass":    "https://user@app.example/path and https://app.example:8443/x",
		"url, short pass": "https://user:abc@app.example/",
		"token name":      `- textbox "Token name" [ref=e5]: ci-deploy`,
		"placeholder":     `- textbox "Password" [ref=e6]: ` + strings.Repeat("•", 20),
		"clock-ish":       "12345678:AA short",
		"bare jwt-ish":    "eyJpZCI6MX0.eyJhIjoxfQ.notsigned",
		"docs url":        "DATABASE_URL=postgresql://postgres:postgres@localhost:5432/app, or mongodb://admin:password@db",
		"templated url":   "https://user:${DB_PASSWORD}@host and https://user:********@registry.example",
		"url, no path":    "https://example.test:8443?email=someone@example.org",
		"kebab sk-":       "install sk-learn-model-selection-helpers today; rk-industries-annual-report-summary",
		"xoxo":            "xoxo-hugs-and-kisses-for-everyone-2024",
		"long token name": `- textbox "Token name" [ref=e4]: production-deploy-pipeline`,
		"tskey docs":      "tskey-auth-XXXX-YYYY",
		"caps word":       "ASIAN" + "DEVELOPMENTBANK",
	} {
		t.Run(name, func(t *testing.T) {
			if found := FindCredentials(text); len(found) != 0 {
				t.Errorf("FindCredentials(%q) = %q, want nothing", text, found)
			}
		})
	}
}

// The two structural rules keep their context readable and take only the
// credential itself.
func TestCredentialsTakeOnlyTheSecretPart(t *testing.T) {
	value := "fake" + strings.Repeat("Q1", 10)
	for name, tc := range map[string]struct{ text, want string }{
		"url password":   {"postgres://app:" + value + "@db.example:5432/app", value},
		"labelled field": {`- textbox "API key" [active] [ref=e4]: ` + value, value},
		"secret field":   {`- textbox "Client secret" [ref=e9]: ` + value + "\n- button", value},
	} {
		t.Run(name, func(t *testing.T) {
			if found := FindCredentials(tc.text); len(found) != 1 || found[0] != tc.want {
				t.Fatalf("FindCredentials = %q, want only the secret part", found)
			}
		})
	}
}
