package jev

import (
	"encoding/json"
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
// page. tree is every node of its aria snapshot, kept only so `--extract` can
// read page lines from it; nothing else in the loop looks at it. It holds the
// yaml tree and nothing else - not the open tabs, whose URLs carry query
// strings, nor the console, nor any other section the driver prints.
type Snapshot struct {
	URL      string
	Title    string
	Modal    string // the dialog description, empty when no dialog is pending
	Elements []Element
	tree     []node
}

// element finds the node a ref names. What the model answers is checked against
// the ACTION SPACE rather than against this, because the action space is what it
// was offered: a ref this snapshot carries but that pruning dropped - a route
// already taken, an element with no accessible name - is still not one to act on.
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

// maxLabel bounds one element's label. A label past this is a paragraph that
// someone gave an aria-label, and the first line of it is the part that says
// what the control does. It is cut here, at the parse, so the option rubric and
// the element the state carries can never disagree about what a control is
// called.
const maxLabel = 200

// roleTextbox is the one role whose value commits on blur rather than on input,
// which is why a fill into it is followed by a Tab.
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

// refRE reads the attribute that gives an element its handle.
var refRE = regexp.MustCompile(`\[ref=([A-Za-z0-9]+)\]`)

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
	lastElement := -1
	for line := range strings.SplitSeq(out, "\n") {
		if header, ok := strings.CutPrefix(line, "### "); ok {
			section = strings.TrimSpace(header)
			// Both blocks reset, so the LAST one of each kind wins. Without this a
			// dialog that was raised and then cleared earlier in the same capture
			// would outrank the block that says it is gone, and the loop would park
			// on a modal nothing can clear.
			switch section {
			case sectionSnapshot:
				snap.Elements, snap.tree, lastElement = nil, nil, -1
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
				// an extract skips it and everything under it rather than sending a
				// value nested below a field it failed to recognize.
				snap.tree = append(snap.tree, node{Depth: len(line) - len(strings.TrimLeft(line, " ")), opaque: true})
				lastElement = -1
				continue
			}
			if !ok {
				// A link's target is the first property under its node line, and a
				// link to a section of this same page is no action at all: following
				// it only scrolls, and the whole page is already in the snapshot. On
				// a long article such links are hundreds of footnotes and a table of
				// contents a read task would walk until its budget ran out, and they
				// would crowd real links out past maxElements. The price is the rare
				// link a script turns into an action through a named fragment - an
				// old-style `href="#loginModal"` modal trigger - which goes with them.
				if target, ok := strings.CutPrefix(strings.TrimSpace(line), "- /url: "); ok &&
					lastElement >= 0 && sectionAnchor(unquoteValue(target)) {
					snap.Elements = snap.Elements[:lastElement]
					lastElement = -1
				}
				continue
			}
			snap.tree = append(snap.tree, n)
			lastElement = -1
			if el, ok := n.element(); ok && len(snap.Elements) < maxElements {
				snap.Elements = append(snap.Elements, el)
				lastElement = len(snap.Elements) - 1
			}
		}
	}
	if sealed := sealEchoedValues(snap.tree); len(sealed) > 0 {
		snap.Elements = slices.DeleteFunc(snap.Elements, func(el Element) bool { return sealed[el.Ref] })
	}
	return snap
}

// sealEchoedValues closes the route a field's value has into ANOTHER node's
// name: a label that holds a field - wrapping it, or pointed at by `for=` or
// aria-labelledby - names the control it labels with the field's current value,
// so `- radio "Other: hunter2"` sits next to `- textbox [ref=e9]: hunter2`. Each
// such node is marked opaque, so an extract skips it, and its ref is returned
// so the action space drops it. Only nodes near the field and no deeper than it
// are checked, because that is where a labelled control sits, and a results
// list further off legitimately repeats what was typed into a search box.
func sealEchoedValues(tree []node) map[string]bool {
	sealed := map[string]bool{}
	for i, field := range tree {
		if !typableRoles[field.Role] {
			continue
		}
		values := fieldValues(tree, i)
		if len(values) == 0 {
			continue
		}
		lo, hi := grandparentSubtree(tree, i)
		for j := lo; j < hi; j++ {
			n := &tree[j]
			if j == i || n.Depth > field.Depth || n.Name == "" {
				continue
			}
			if slices.ContainsFunc(values, func(v string) bool { return strings.Contains(n.Name, v) }) {
				n.opaque = true
				if ref := refRE.FindStringSubmatch(n.Attrs); ref != nil {
					sealed[ref[1]] = true
				}
			}
		}
	}
	return sealed
}

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

// grandparentSubtree is the index range of the subtree two levels above node i,
// or the whole tree when i sits too close to the top to have one.
func grandparentSubtree(tree []node, i int) (int, int) {
	lo := i
	for range 2 {
		k := lo - 1
		for k >= 0 && tree[k].Depth >= tree[lo].Depth {
			k--
		}
		if k < 0 {
			return 0, len(tree)
		}
		lo = k
	}
	hi := lo + 1
	for hi < len(tree) && tree[hi].Depth > tree[lo].Depth {
		hi++
	}
	return lo, hi
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

// sectionAnchor reports whether a link target is a section of the current
// page. A bare `#` is a script's click handler and `#/` or `#!` a single-page
// app's route - both real actions - so only a named fragment counts.
func sectionAnchor(target string) bool {
	name, ok := strings.CutPrefix(target, "#")
	return ok && name != "" && name[0] != '/' && name[0] != '!'
}

// isNodeLine reports whether a line of the snapshot section is a node line, as
// opposed to a property (`- /url: ...`) or the yaml fence.
func isNodeLine(line string) bool {
	body, ok := strings.CutPrefix(strings.TrimLeft(line, " "), "- ")
	return ok && !strings.HasPrefix(body, "/")
}
