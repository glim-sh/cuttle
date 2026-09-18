// Package mask scrubs credential-shaped text that cuttle is about to print or
// log. It is shared because the daemon and the CLI both emit URLs the browser
// has been to, and a rule this specific drifts the moment it exists twice: the
// leak it was written for was a routine retry log carrying
// `remix_userkey=25039df9...` in the URL it was retrying, and an OAuth callback
// an agent lands on carries the same shape in `?code=`.
package mask

import (
	"regexp"
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
