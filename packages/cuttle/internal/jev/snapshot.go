package jev

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// maxElements bounds how much of one page becomes an action space. It is not
// TypeSafe's Choice cap (255 options) - the options are grouped, so the cap that
// bites is the size of the whole pruned list, and a page with more interactive
// controls than this is one where the next action is not going to be found by
// offering more of them. Real pages reach four-digit refs, so this truncates.
const maxElements = 500

// Element is one interactive node of the page, and - apart from what `--extract`
// explicitly asks for - the ONLY page data that ever leaves this process. Field
// VALUES, page body text and the URLs behind links are deliberately absent.
type Element struct {
	Ref   string `json:"ref"`
	Role  string `json:"role"`
	Label string `json:"label"`
}

// Snapshot is what one `playwright-cli snapshot` invocation tells us about the
// page. Raw is the driver's whole output, kept only so `--extract` can read page
// lines from it; nothing else in the loop looks at it.
type Snapshot struct {
	URL      string
	Title    string
	Modal    string // the dialog description, empty when no dialog is pending
	Elements []Element
	Raw      string
}

// offered reports whether the ref is one this snapshot put in front of the
// model. It gates the single answer that decides what gets clicked or filled:
// these are the only refs the model is ever given, so anything else is a
// malformed answer aimed at an element nobody vouched for - a disabled control
// the filter dropped, or, when the field is missing altogether, the empty string.
func (s Snapshot) offered(ref string) bool {
	return slices.ContainsFunc(s.Elements, func(el Element) bool { return sameRef(el.Ref, ref) })
}

func (s Snapshot) element(ref string) (Element, bool) {
	i := slices.IndexFunc(s.Elements, func(el Element) bool { return sameRef(el.Ref, ref) })
	if i < 0 {
		return Element{}, false
	}
	return s.Elements[i], true
}

// signature is what makes "the page stopped changing" decidable: the identity of
// the page plus every handle on it. A navigation, a re-render that mints new
// refs, and a control appearing or disappearing all change it; a spinner
// rotating does not.
func (s Snapshot) signature() string {
	var b strings.Builder
	b.WriteString(s.URL)
	for _, el := range s.Elements {
		b.WriteString("\x00")
		b.WriteString(el.Ref)
		b.WriteString(el.Role)
		b.WriteString(el.Label)
	}
	return b.String()
}

// frameRE splits the frame qualifier playwright adds to a ref after a navigation
// (`f1e2`) off the element part (`e2`).
var frameRE = regexp.MustCompile(`^f\d+(e\d+)$`)

// bareRef drops the frame qualifier. cuttle's mux hands out frame-prefixed refs,
// but a model echoing an option back, or a human retyping one, produces the bare
// form just as often, and the two name the same element.
func bareRef(ref string) string {
	if m := frameRE.FindStringSubmatch(ref); m != nil {
		return m[1]
	}
	return ref
}

func sameRef(a, b string) bool { return a == b || bareRef(a) == bareRef(b) }

// interactiveRoles are the ARIA roles worth offering as a next action. Roles
// that only carry text (paragraph, listitem, heading, generic) are dropped: they
// cost options without being actionable, and their text is exactly what must not
// be sent.
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
	roleTextbox:          true,
	"treeitem":           true,
	"disclosuretriangle": true,
}

// roleTextbox is the one role whose value commits on blur rather than on input,
// which is why a fill into it is followed by a Tab.
const roleTextbox = "textbox"

// typableRoles are the roles a prepared value can be typed into.
var typableRoles = map[string]bool{roleTextbox: true, "searchbox": true, "combobox": true}

// nodeRE matches one node line of the aria snapshot: an optional quoted
// accessible name, then the bracketed attributes in whatever order playwright
// emitted them. Both halves are optional because plenty of real nodes have
// neither (`- text: Username`), and those simply fail the ref lookup below.
//
//   - textbox "Username" [ref=e7]
//   - button "Continue" [disabled] [ref=f1e10]
//   - link "Home" [ref=f1e3] [cursor=pointer]:
var nodeRE = regexp.MustCompile(`^\s*-\s+([a-z]+)(?:\s+"((?:[^"\\]|\\.)*)")?((?:\s+\[[^\]]*\])*)`)

// refRE reads the attribute that gives an element its handle.
var refRE = regexp.MustCompile(`\[ref=([A-Za-z0-9]+)\]`)

// ParseSnapshot reads the combined output of `playwright-cli snapshot`. It takes
// the whole output rather than `--raw` output because the sections around the
// yaml carry the two things a raw snapshot drops: the page identity, and the
// `### Modal state` block that means the renderer is parked behind a native
// dialog. A modal snapshot is also an ERROR exit from playwright-cli, so the
// caller must hand us output it would otherwise have thrown away.
//
// One capture can hold SEVERAL verbs' output, each with its own page block, so
// the last block of each kind wins: that is the page as the driver left it, and
// the earlier ones describe pages that no longer exist.
func ParseSnapshot(out string) Snapshot {
	snap := Snapshot{Raw: out}
	section := ""
	for line := range strings.SplitSeq(out, "\n") {
		if header, ok := strings.CutPrefix(line, "### "); ok {
			section = strings.TrimSpace(header)
			// Both blocks reset, so the LAST one of each kind wins. Without this a
			// dialog that was raised and then cleared earlier in the same capture
			// would outrank the block that says it is gone, and the loop would park
			// on a modal nothing can clear.
			switch section {
			case sectionSnapshot:
				snap.Elements = nil
			case sectionModal:
				snap.Modal = ""
			}
			continue
		}
		// The page identity is read outside its section too: a snapshot parked
		// behind a file chooser prints `- Page URL:` as a bare first line with no
		// `### Page` header at all, and dropping the URL there loses the one field
		// the handoff brief is built from.
		if url, ok := strings.CutPrefix(line, "- Page URL: "); ok {
			snap.URL, snap.Title = strings.TrimSpace(url), ""
			continue
		}
		if title, ok := strings.CutPrefix(line, "- Page Title: "); ok {
			snap.Title = strings.TrimSpace(title)
			continue
		}
		switch section {
		case sectionModal:
			if text := strings.TrimSpace(line); text != "" && snap.Modal == "" {
				snap.Modal = strings.TrimPrefix(text, "- ")
			}
		case sectionSnapshot:
			if el, ok := parseNode(line); ok && len(snap.Elements) < maxElements {
				snap.Elements = append(snap.Elements, el)
			}
		}
	}
	return snap
}

const (
	sectionSnapshot = "Snapshot"
	sectionModal    = "Modal state"
)

func parseNode(line string) (Element, bool) {
	m := nodeRE.FindStringSubmatch(line)
	if m == nil {
		return Element{}, false
	}
	role, label, attrs := m[1], m[2], m[3]
	// [disabled] is the one state playwright spells out that makes an element
	// unactionable. Invisible nodes need no filter: the accessibility tree does
	// not carry them in the first place.
	if !interactiveRoles[role] || strings.Contains(attrs, "[disabled]") {
		return Element{}, false
	}
	ref := refRE.FindStringSubmatch(attrs)
	if ref == nil {
		return Element{}, false
	}
	return Element{Ref: ref[1], Role: role, Label: strings.TrimSpace(unquoteLabel(label))}, true
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
