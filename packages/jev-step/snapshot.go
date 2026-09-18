package main

import (
	"regexp"
	"strconv"
	"strings"
)

// maxElements is Jev's hard cap on the number of options in a Choice question.
// A page with more interactive elements than this is truncated rather than
// rejected: the first 255 in document order are the ones near the top of the
// page, which is where the next action almost always is.
const maxElements = 255

// element is one interactive node of the page, and the ONLY page data that ever
// leaves this process. Field values, page text and URLs the elements point at
// are deliberately absent - see the privacy note in README.md.
type element struct {
	ID    string `json:"id"`
	Role  string `json:"role"`
	Label string `json:"label"`
}

// snapshot is what one `playwright-cli snapshot` invocation tells us about the
// page.
type snapshot struct {
	URL      string
	Title    string
	Modal    string // the dialog description, empty when no dialog is pending
	Elements []element
}

// interactiveRoles are the ARIA roles worth offering as a next action. Roles
// that only carry text (paragraph, listitem, heading, generic) are dropped:
// they cost Jev options without being clickable, and their text is exactly what
// must not be sent.
var interactiveRoles = map[string]bool{
	"button":             true,
	"checkbox":           true,
	"combobox":           true,
	"link":               true,
	"listbox":            true,
	"menuitem":           true,
	"menuitemcheckbox":   true,
	"menuitemradio":      true,
	"option":             true,
	"radio":              true,
	"searchbox":          true,
	"slider":             true,
	"spinbutton":         true,
	"switch":             true,
	"tab":                true,
	"textbox":            true,
	"treeitem":           true,
	"disclosuretriangle": true,
}

// nodeRE matches one node line of the aria snapshot: an optional quoted
// accessible name, then the bracketed attributes in whatever order playwright
// emitted them. Both halves are optional because plenty of real nodes have
// neither (`- text: Username`), and those simply fail the ref lookup below.
//
//   - textbox "Username" [ref=e7]
//   - button "Continue" [disabled] [ref=f1e10]
//   - link "Home" [ref=f1e3] [cursor=pointer]:
var nodeRE = regexp.MustCompile(`^\s*-\s+([a-z]+)(?:\s+"((?:[^"\\]|\\.)*)")?((?:\s+\[[^\]]*\])*)`)

// refRE and disabledRE read the two attributes that decide an element's fate.
// Refs are frame-qualified after a navigation (`f1e10`), not just `e10`.
var (
	refRE      = regexp.MustCompile(`\[ref=([A-Za-z0-9]+)\]`)
	disabledRE = regexp.MustCompile(`\[disabled\]`)
)

// parseSnapshot reads the combined output of `playwright-cli snapshot`. It
// takes the whole output rather than `--raw` output because the sections around
// the yaml carry the two things a raw snapshot drops: the page identity, and
// the `### Modal state` block that means the renderer is parked behind a native
// dialog. A modal snapshot is also an ERROR exit from playwright-cli, so the
// caller must hand us output it would otherwise have thrown away.
func parseSnapshot(out string) snapshot {
	var snap snapshot
	section := ""
	for line := range strings.SplitSeq(out, "\n") {
		if header, ok := strings.CutPrefix(line, "### "); ok {
			section = strings.TrimSpace(header)
			continue
		}
		switch section {
		case "Page":
			if url, ok := strings.CutPrefix(line, "- Page URL: "); ok {
				snap.URL = strings.TrimSpace(url)
			}
			if title, ok := strings.CutPrefix(line, "- Page Title: "); ok {
				snap.Title = strings.TrimSpace(title)
			}
		case "Modal state":
			if text := strings.TrimSpace(line); text != "" && snap.Modal == "" {
				snap.Modal = strings.TrimPrefix(text, "- ")
			}
		case "Snapshot":
			if el, ok := parseNode(line); ok && len(snap.Elements) < maxElements {
				snap.Elements = append(snap.Elements, el)
			}
		}
	}
	return snap
}

func parseNode(line string) (element, bool) {
	m := nodeRE.FindStringSubmatch(line)
	if m == nil {
		return element{}, false
	}
	role, label, attrs := m[1], m[2], m[3]
	if !interactiveRoles[role] || disabledRE.MatchString(attrs) {
		return element{}, false
	}
	ref := refRE.FindStringSubmatch(attrs)
	if ref == nil {
		return element{}, false
	}
	return element{ID: ref[1], Role: role, Label: unquoteLabel(label)}, true
}

// unquoteLabel undoes playwright's double-quoted yaml escaping. Go's unquoting
// rules are a superset of what it emits for a label, and a label it somehow
// cannot parse is still better shown escaped than dropped.
func unquoteLabel(label string) string {
	if !strings.Contains(label, `\`) {
		return label
	}
	if unquoted, err := strconv.Unquote(`"` + label + `"`); err == nil {
		return unquoted
	}
	return label
}
