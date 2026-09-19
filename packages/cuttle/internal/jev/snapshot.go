package jev

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxElements bounds how much of one page becomes an action space. It is not
// TypeSafe's Choice cap (255 options) - the options are grouped, so the cap that
// bites is the size of the whole pruned list, and a page with more interactive
// controls than this is one where the next action is not going to be found by
// offering more of them. Real pages reach four-digit refs, so this truncates.
const maxElements = 500

// Element is one interactive node of the page. Field VALUES and the URLs behind
// links are deliberately absent: a filled field says only that it is filled,
// which is also all it can say after a `{{cuttle:NAME}}` fill, when its value
// is the substituted secret.
type Element struct {
	Ref   string `json:"ref"`
	Role  string `json:"role"`
	Label string `json:"label"`
	// Section is the nearest landmark the element sits in, such as
	// "banner: Site" or "main". Without it a site-wide search box
	// and a form's own box read as the same control.
	Section string `json:"section,omitempty"`
	// State is what the snapshot says about the control's current state:
	// filled, checked, mixed, expanded, selected, active (focused).
	State string `json:"state,omitempty"`
}

// heading is one heading of the page with its level, the outline a reader
// navigates by and the one place a "reach its X section" task can be judged.
type heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
}

// maxHeadings bounds the outline sent with a page. Past this many the page is
// an index, and its text carries the rest.
const maxHeadings = 40

// rubric is how an element is named in an option: its section, role and label.
func (el Element) rubric() string {
	if el.Section == "" {
		return el.Role + ": " + el.Label
	}
	return "[" + el.Section + "] " + el.Role + ": " + el.Label
}

// Snapshot is what one `playwright-cli snapshot` invocation tells us about the
// page. tree is every node of its aria snapshot, which pageLines renders as
// the page's text. It holds the yaml tree and nothing else - not the open
// tabs, whose URLs carry query strings, nor the console, nor any other section
// the driver prints.
type Snapshot struct {
	URL      string
	Title    string
	Modal    string // the dialog description, empty when no dialog is pending
	Headings []heading
	Elements []Element
	tree     []node
}

// element finds the node a ref names. What the model answers is checked against
// the ACTION SPACE rather than against this, because the action space is what it
// was offered: a ref this snapshot carries but the action space left out - an
// element with no accessible name - is still not one to act on.
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
func (s Snapshot) signature() string { return s.URL + s.handles() }

// handles is every element on the page, without its identity: the same string
// for the same DOM whatever the address bar says.
func (s Snapshot) handles() string {
	var b strings.Builder
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

// maxLabel bounds one element's label. A label past this is a paragraph that
// someone gave an aria-label, and the first line of it is the part that says
// what the control does. It is cut here, at the parse, so the option rubric and
// the element the state carries can never disagree about what a control is
// called.
const maxLabel = 200

// roleTextbox is the plain text-field role.
const roleTextbox = "textbox"

// typableRoles are the roles a prepared value can be typed into.
var typableRoles = map[string]bool{roleTextbox: true, "searchbox": true, "combobox": true}

// attrRE is one bracketed attribute playwright appends after a node's name:
// `[ref=f1e3]`, `[level=2]`, `[box=0,0,10,10]`, `[disabled]`. Its value can
// hold no quote or slash, so a bracket inside a name - `"Card [required"` - can
// never be taken for one.
var attrRE = regexp.MustCompile(`^ \[[a-z-]+(?:=[^\]\s"/]*)?\]$`)

// cutAttrs splits a key into what precedes its attributes and the attributes
// themselves. They are only ever appended, so they are read off the END.
func cutAttrs(key string) (string, string) {
	end := len(key)
	for {
		i := strings.LastIndex(key[:end], " [")
		if i < 0 || !attrRE.MatchString(key[i:end]) {
			return key[:end], key[end:]
		}
		end = i
	}
}

// headRE splits what is left of a key into its role and its accessible name.
var headRE = regexp.MustCompile(`^([a-z]+)(?:\s+(.+))?$`)

// refRE reads the attribute that gives an element its handle, levelRE the one
// that gives a heading its depth in the outline.
var (
	refRE   = regexp.MustCompile(`\[ref=([A-Za-z0-9]+)\]`)
	levelRE = regexp.MustCompile(`\[level=(\d+)\]`)
)

// node is one line of the aria snapshot, with playwright's yaml quoting undone.
// The shapes it comes in, from playwright's ariaSnapshotRenderer:
//
//   - textbox "Username" [ref=e7]
//   - link "Home" [ref=f1e3] [cursor=pointer]:
//   - paragraph [ref=e9]: Some text
//   - 'link "flate: avoid FMA" [ref=e40]'
//   - 'textbox "Password: required" [ref=e8]': "hunter2 #1"
//
// The whole key is single-quoted (a doubled single quote escaping one) whenever
// it would not otherwise read back as a yaml key - most commonly a name holding
// ": ". The name inside it is a JSON string, or bare when it starts and ends
// with a slash, and a value after the key is double-quoted with backslash
// escapes when it needs quoting. Property lines (`- /url: ...`) and anything
// that is not a node line do not parse.
type node struct {
	Depth int // indentation, which is how the tree nests
	Role  string
	Name  string
	Attrs string
	Value string // the node's own text after the key, empty when it has none
	// opaque marks a node line that did not parse. Nothing of it or under it is
	// read as page text.
	opaque bool
}

func parseLine(line string) (node, bool) {
	body := strings.TrimLeft(line, " ")
	depth := len(line) - len(body)
	body, ok := strings.CutPrefix(body, "- ")
	if !ok {
		return node{}, false
	}
	key, rest, ok := cutKey(body)
	if !ok {
		return node{}, false
	}
	n := node{Depth: depth}
	if value, ok := strings.CutPrefix(rest, ": "); ok {
		n.Value = unquoteValue(strings.TrimSpace(value))
	} else if rest != "" && rest != ":" {
		return node{}, false
	}
	key, n.Attrs = cutAttrs(key)
	m := headRE.FindStringSubmatch(key)
	if m == nil {
		return node{}, false
	}
	n.Role = m[1]
	switch name := m[2]; {
	case name == "":
	case strings.HasPrefix(name, `"`):
		if err := json.Unmarshal([]byte(name), &n.Name); err != nil {
			return node{}, false
		}
	case strings.HasPrefix(name, "/") && strings.HasSuffix(name, "/"):
		// A name that starts and ends with a slash is printed bare, because the
		// same syntax spells a regex in an assertion.
		n.Name = name
	default:
		return node{}, false
	}
	return n, true
}

// cutKey splits a node line's body into its key, unquoted, and what follows
// it: nothing, the ":" that opens its children, or ": " and its text.
func cutKey(body string) (string, string, bool) {
	quoted, isQuoted := strings.CutPrefix(body, "'")
	if !isQuoted {
		// An unquoted key never holds ": " or ends in ":" - playwright quotes it
		// when it would - so the first of either is where the key ends.
		if i := strings.Index(body, ": "); i >= 0 {
			return body[:i], body[i:], true
		}
		if k, found := strings.CutSuffix(body, ":"); found {
			return k, ":", true
		}
		return body, "", true
	}
	var b strings.Builder
	for i := 0; i < len(quoted); i++ {
		switch {
		case quoted[i] != '\'':
			b.WriteByte(quoted[i])
		case i+1 < len(quoted) && quoted[i+1] == '\'':
			b.WriteByte('\'')
			i++
		default:
			return b.String(), quoted[i+1:], true
		}
	}
	return "", "", false
}

// unquoteValue undoes the double-quoted form of a node's text. Playwright
// escapes a C1 control as `\x85`, which in yaml is that code point; Go reads
// `\x` as a raw byte, so those become `\u` first. A value that still does not
// unquote is better shown escaped than dropped.
func unquoteValue(value string) string {
	if !strings.HasPrefix(value, `"`) {
		return value
	}
	value = hexEscapeRE.ReplaceAllStringFunc(value, func(esc string) string {
		if esc == `\\` {
			return esc
		}
		return `\u00` + esc[2:]
	})
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return value
}

// hexEscapeRE matches an escaped backslash too, so a `\\x41` - a literal
// backslash followed by "x41" - is consumed as the pair it is.
var hexEscapeRE = regexp.MustCompile(`\\(?:\\|x[0-9a-fA-F]{2})`)

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
	var snap Snapshot
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
				snap.Elements, snap.tree = nil, nil
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
			n, ok := parseLine(line)
			if !ok && isNodeLine(line) {
				// A node line this parser cannot read is kept as an opaque node, so
				// nothing of it or under it is read as page text, rather than sending
				// a value nested below a field it failed to recognize.
				snap.tree = append(snap.tree, node{Depth: len(line) - len(strings.TrimLeft(line, " ")), opaque: true})
				continue
			}
			if !ok {
				continue
			}
			snap.tree = append(snap.tree, n)
			if el, ok := n.element(); ok {
				snap.Elements = append(snap.Elements, el)
			}
		}
	}
	redacted := redactEchoedValues(snap.tree)
	for i := range snap.Elements {
		if name, ok := redacted[snap.Elements[i].Ref]; ok {
			snap.Elements[i].Label = truncate(name, maxLabel)
		}
	}
	for _, n := range snap.tree {
		if n.Role == "heading" && !n.opaque && len(snap.Headings) < maxHeadings {
			level := 0
			if m := levelRE.FindStringSubmatch(n.Attrs); m != nil {
				level, _ = strconv.Atoi(m[1])
			}
			snap.Headings = append(snap.Headings, heading{Level: level, Text: truncate(strings.TrimSpace(n.Name), maxLabel)})
		}
	}
	placed := placeElements(snap.tree)
	for i := range snap.Elements {
		p := placed[snap.Elements[i].Ref]
		snap.Elements[i].Section, snap.Elements[i].State = p.Section, p.State
	}
	if inside := openDialogRefs(snap.tree); inside != nil {
		snap.Elements = slices.DeleteFunc(snap.Elements, func(el Element) bool { return !inside[el.Ref] })
	}
	if len(snap.Elements) > maxElements {
		snap.Elements = snap.Elements[:maxElements]
	}
	return snap
}

// landmarkRoles are the containers that tell one part of a page from another.
var landmarkRoles = map[string]bool{
	"banner": true, "main": true, "navigation": true, "search": true, "form": true, "region": true,
	"complementary": true, "contentinfo": true, "dialog": true, "alertdialog": true,
}

// maxSection bounds a landmark's name inside a section. It is a hint, not a
// label, and every option under that landmark repeats it.
const maxSection = 60

// stateAttrs are the attributes that carry a control's state, and the word each
// is offered as. [active] is the focus, which on a typable control is where
// Enter would land.
var stateAttrs = [][2]string{
	{"[checked=mixed]", "mixed"},
	{"[checked]", "checked"},
	{"[expanded]", "expanded"},
	{"[selected]", "selected"},
	{"[pressed]", "pressed"},
	{"[active]", "active"},
}

// placeElements finds, for every ref in the tree, the landmark it sits in and
// its state.
func placeElements(tree []node) map[string]Element {
	placed := map[string]Element{}
	var landmarks []node
	for i, n := range tree {
		for len(landmarks) > 0 && landmarks[len(landmarks)-1].Depth >= n.Depth {
			landmarks = landmarks[:len(landmarks)-1]
		}
		if ref := refRE.FindStringSubmatch(n.Attrs); ref != nil && !n.opaque {
			var el Element
			if len(landmarks) > 0 {
				el.Section = sectionName(landmarks[len(landmarks)-1])
			}
			var state []string
			for _, a := range stateAttrs {
				if strings.Contains(n.Attrs, a[0]) {
					state = append(state, a[1])
				}
			}
			if typableRoles[n.Role] && len(fieldValues(tree, i)) > 0 {
				state = append([]string{"filled"}, state...)
			}
			el.State = strings.Join(state, ", ")
			placed[ref[1]] = el
		}
		if landmarkRoles[n.Role] {
			if n.opaque {
				n.Name = ""
			}
			landmarks = append(landmarks, n)
		}
	}
	return placed
}

// sectionName is a landmark as a section prefix. Brackets are dropped from its
// name so it cannot close the prefix early.
func sectionName(n node) string {
	name := strings.Join(strings.Fields(strings.NewReplacer("[", "", "]", "").Replace(n.Name)), " ")
	if name == "" {
		return n.Role
	}
	return n.Role + ": " + truncate(name, maxSection)
}

// dialogRoles are the landmarks a modal is scoped to, and the ones that, like a
// form, group a box with the button Enter would press.
var dialogRoles = map[string]bool{"dialog": true, "alertdialog": true}

// openDialogRefs returns the refs inside the open dialog, or nil when there is
// none. A page's modal keeps the background in the aria snapshot but intercepts
// every click on it, so offering the background only buys 5s click timeouts.
// Playwright prints no aria-modal marker; the one signal it gives is [active],
// the focused element, which a modal holds on itself or on a control inside it.
// The last such dialog wins, being the innermost or the one stacked on top.
func openDialogRefs(tree []node) map[string]bool {
	var refs map[string]bool
	for i, n := range tree {
		if !dialogRoles[n.Role] {
			continue
		}
		end := i + 1
		for end < len(tree) && tree[end].Depth > n.Depth {
			end++
		}
		if !slices.ContainsFunc(tree[i:end], func(d node) bool { return strings.Contains(d.Attrs, "[active]") }) {
			continue
		}
		refs = map[string]bool{}
		for _, d := range tree[i+1 : end] {
			if ref := refRE.FindStringSubmatch(d.Attrs); ref != nil {
				refs[ref[1]] = true
			}
		}
	}
	// A focused dialog with nothing to click leaves the page's own controls as
	// the only way on, not an empty choice.
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// typedValueMark stands in for a field's value wherever another node's name
// repeats it, so the model can still see that a suggestion or a label matches
// what was typed without seeing what was typed.
const typedValueMark = "<<typed value>>"

// redactEchoedValues closes the route a field's value has into ANOTHER node's
// name: a label that holds a field - wrapping it, or pointed at by `for=` or
// aria-labelledby - names the control it labels with the field's current
// value, so `- radio "Other: hunter2"` sits next to `- textbox [ref=e9]:
// hunter2`, and a suggestion list echoes the query as `option "hunter2
// widgets"`. Every node name that holds a filled field's value as whole words
// has it replaced with typedValueMark, in place - the node stays, so a
// suggestion is still there to click - and the refs whose names changed are
// returned with their new names, so the elements follow.
func redactEchoedValues(tree []node) map[string]string {
	var values []string
	for i, n := range tree {
		if typableRoles[n.Role] {
			values = append(values, fieldValues(tree, i)...)
		}
	}
	redacted := map[string]string{}
	for i := range tree {
		n := &tree[i]
		for _, v := range values {
			name, found := replaceWords(n.Name, v, typedValueMark)
			if !found {
				continue
			}
			n.Name = name
			if ref := refRE.FindStringSubmatch(n.Attrs); ref != nil {
				redacted[ref[1]] = name
			}
		}
	}
	return redacted
}

// replaceWords replaces every occurrence of words in s that has no letter or
// digit running on at either end - "e" typed into one box is not in "Home" -
// and reports whether there was one.
func replaceWords(s, words, mark string) (string, bool) {
	var b strings.Builder
	found := false
	for from := 0; ; {
		i := strings.Index(s[from:], words)
		if i < 0 {
			b.WriteString(s[from:])
			return b.String(), found
		}
		start, end := from+i, from+i+len(words)
		before, _ := utf8.DecodeLastRuneInString(s[:start])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if (start == 0 || !isWordRune(before)) && (end == len(s) || !isWordRune(after)) {
			b.WriteString(s[from:start])
			b.WriteString(mark)
			found = true
			from = end
			continue
		}
		b.WriteString(s[from : start+1])
		from = start + 1
	}
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// fieldValues is what a field renders as its value: its own text, or the
// `- text:` child it prints instead when it also has a placeholder. Whitespace
// is collapsed the way an accessible name collapses it.
func fieldValues(tree []node, i int) []string {
	var values []string
	for j := i; j < len(tree) && (j == i || tree[j].Depth > tree[i].Depth); j++ {
		if v := strings.Join(strings.Fields(tree[j].Value), " "); v != "" {
			values = append(values, v)
		}
	}
	return values
}

const (
	sectionSnapshot = "Snapshot"
	sectionModal    = "Modal state"
)

func (n node) element() (Element, bool) {
	// [disabled] is the one state playwright spells out that makes an element
	// unactionable. Invisible nodes need no filter: the accessibility tree does
	// not carry them in the first place.
	if !interactiveRoles[n.Role] || strings.Contains(n.Attrs, "[disabled]") {
		return Element{}, false
	}
	ref := refRE.FindStringSubmatch(n.Attrs)
	if ref == nil {
		return Element{}, false
	}
	return Element{Ref: ref[1], Role: n.Role, Label: truncate(strings.TrimSpace(n.Name), maxLabel)}, true
}

// isNodeLine reports whether a line of the snapshot section is a node line, as
// opposed to a property (`- /url: ...`) or the yaml fence.
func isNodeLine(line string) bool {
	body, ok := strings.CutPrefix(strings.TrimLeft(line, " "), "- ")
	return ok && !strings.HasPrefix(body, "/")
}
