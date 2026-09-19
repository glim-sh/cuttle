package jev

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

func parseFixture(t *testing.T, name string) Snapshot {
	t.Helper()
	return ParseSnapshot(readFixture(t, name))
}

func TestParseSnapshotReadsThePageAndItsControls(t *testing.T) {
	snap := parseFixture(t, "signin.snapshot")

	if got, want := snap.URL, "http://127.0.0.1:8799/"; got != want {
		t.Errorf("url: got %q, want %q", got, want)
	}
	if got, want := snap.Title, "Cuttle demo shop"; got != want {
		t.Errorf("title: got %q, want %q", got, want)
	}
	want := []Element{
		{Ref: "f2e3", Role: "link", Label: "Home", Section: "banner"},
		{Ref: "f2e4", Role: "link", Label: "Cart (0)", Section: "banner"},
		{Ref: "f2e7", Role: "textbox", Label: "Username"},
		{Ref: "f2e8", Role: "textbox", Label: "Password"},
		{Ref: "f2e9", Role: "combobox"},
		{Ref: "f2e10", Role: "checkbox", Label: "Remember me"},
		{Ref: "f2e11", Role: "button", Label: "Login"},
		{Ref: "f2e12", Role: "button", Label: "Help"},
	}
	if len(snap.Elements) != len(want) {
		t.Fatalf("elements: got %d %+v, want %d", len(snap.Elements), snap.Elements, len(want))
	}
	for i, el := range want {
		if snap.Elements[i] != el {
			t.Errorf("element %d: got %+v, want %+v", i, snap.Elements[i], el)
		}
	}
}

// The text of a page is the one thing that must never reach a decision model,
// and the snapshot is where it would get in.
func TestParseSnapshotDropsTextOnlyNodes(t *testing.T) {
	for _, el := range parseFixture(t, "signin.snapshot").Elements {
		if el.Role == "paragraph" || el.Role == "listitem" || el.Role == "generic" {
			t.Errorf("a text-carrying node reached the action space: %+v", el)
		}
	}
}

func TestParseSnapshotDropsDisabledControlsAndUnescapesLabels(t *testing.T) {
	snap := parseFixture(t, "checkout.snapshot")
	if _, ok := snap.element("f1e10"); ok {
		t.Error("the disabled Continue button was kept as an action")
	}
	el, ok := snap.element("f1e12")
	if !ok {
		t.Fatal("the Help link was dropped")
	}
	if got, want := el.Label, `Help "centre"`; got != want {
		t.Errorf("label: got %q, want %q", got, want)
	}
}

// cuttle's mux hands out frame-prefixed refs, but an answer echoing one back, or
// a person retyping it, produces the bare form just as often.
func TestElementAcceptsBothRefForms(t *testing.T) {
	snap := parseFixture(t, "signin.snapshot")
	for _, ref := range []string{"f2e11", "e11"} {
		if _, ok := snap.element(ref); !ok {
			t.Errorf("%q was not recognized as the Login button", ref)
		}
	}
	for _, ref := range []string{"", "f2e99", "e99", "f9e11x"} {
		if _, ok := snap.element(ref); ok {
			t.Errorf("%q was accepted, but this page has no such element", ref)
		}
	}
}

// A dialog parks the renderer and makes `snapshot` an ERROR exit, with the block
// naming it riding along on that error. A caller that throws the output away
// loses the only thing that says how to recover.
func TestParseSnapshotReadsAModalOffAnErrorExit(t *testing.T) {
	cases := map[string]struct{ wantModal, wantURL string }{
		"modal_beforeunload.snapshot": {
			wantModal: `["beforeunload" dialog with message ""]: can be handled by dialog-accept or dialog-dismiss`,
		},
		// This one carries no `### Page` header at all: the URL is a bare first
		// line, and the handoff brief is built from it.
		"modal_filechooser.snapshot": {
			wantModal: "[File chooser]: can be handled by upload",
			wantURL:   "https://portal.example.gov/app/entrega/v2026#!/rosto/inicio/@ts!1700000000000",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			snap := parseFixture(t, name)
			if snap.Modal != tc.wantModal {
				t.Errorf("modal: got %q, want %q", snap.Modal, tc.wantModal)
			}
			if snap.URL != tc.wantURL {
				t.Errorf("url: got %q, want %q", snap.URL, tc.wantURL)
			}
		})
	}
}

// One capture can hold several verbs' output. The last page block is the page as
// the driver left it; the earlier ones describe pages that no longer exist.
func TestParseSnapshotTakesTheLastPageBlock(t *testing.T) {
	snap := parseFixture(t, "download_click_sequence.snapshot")
	if got, want := snap.URL, "https://vat.example.gov/portal/obter-comprovativo#!?ano=2026"; got != want {
		t.Errorf("url: got %q, want %q", got, want)
	}
	if got, want := snap.Title, "VAT Return"; got != want {
		t.Errorf("title: got %q, want %q", got, want)
	}
}

// The modal block follows the same rule as the page block: a dialog raised and
// then cleared earlier in the same capture must not outrank the block that says
// it is gone, or the loop parks on a modal nothing can clear.
func TestParseSnapshotTakesTheLastModalBlock(t *testing.T) {
	snap := ParseSnapshot(strings.Join([]string{
		"### Modal state",
		`- ["beforeunload" dialog with message ""]: can be handled by dialog-accept or dialog-dismiss`,
		"### Page",
		"- Page URL: https://shop.example/cart",
		"### Modal state",
		"### Snapshot",
		"```yaml",
		`- button "Checkout" [ref=e1]`,
		"```",
	}, "\n"))
	if snap.Modal != "" {
		t.Errorf("modal: got %q, want the cleared dialog to win", snap.Modal)
	}
	if len(snap.Elements) != 1 {
		t.Errorf("elements: got %d, want the page behind the cleared dialog", len(snap.Elements))
	}
}

// Roughly half of real captures spill the tree to a file and print a link to it,
// and a 404 has neither a title nor an inline page. Zero elements must be an
// ordinary, readable result rather than a parse failure.
func TestParseSnapshotSurvivesPagesWithNoInlineTree(t *testing.T) {
	blank := parseFixture(t, "blank_tab.snapshot")
	if blank.URL != "about:blank" || len(blank.Elements) != 0 {
		t.Errorf("blank tab: got %q with %d elements", blank.URL, len(blank.Elements))
	}
	stale := parseFixture(t, "timeout_stale_ref.snapshot")
	if got, want := stale.URL, "https://shop.example/portal/SelfRegister"; got != want {
		t.Errorf("stale-ref capture url: got %q, want %q", got, want)
	}
	if len(stale.Elements) != 0 {
		t.Errorf("a capture whose tree spilled to a file parsed %d elements", len(stale.Elements))
	}
}

func TestParseSnapshotReadsAPageHeaderWithNoTitle(t *testing.T) {
	snap := parseFixture(t, "notfound_404.snapshot")
	if got, want := snap.URL, "https://portal.example.gov/notificacoes/home"; got != want {
		t.Errorf("url: got %q, want %q", got, want)
	}
	if snap.Title != "" {
		t.Errorf("title: got %q, want empty - the capture carries no Page Title line", snap.Title)
	}
	if _, ok := snap.element("f1e23"); !ok {
		t.Error("the banner links the session recovered through were not parsed")
	}
}

// A link to a section of the page it is on only scrolls, and the snapshot
// already holds the whole page - so it is no action. A bare `#` is a script's
// click handler, and `#/` or `#!` a single-page app's route: those are real.
func TestParseSnapshotDropsLinksToASectionOfThePage(t *testing.T) {
	snap := parseFixture(t, "notfound_404.snapshot")
	for _, ref := range []string{"f1e3", "f1e4"} {
		if el, ok := snap.element(ref); ok {
			t.Errorf("a skip link was kept as an action: %+v", el)
		}
	}
	if _, ok := snap.element("f1e44"); !ok {
		t.Error(`the "#" menu link, a click handler, was dropped`)
	}
	for target, want := range map[string]bool{"#Rediscovery": true, "#cite_note-1": true, "#": false, "#/inbox": false, "#!/home": false, "/wiki/X#y": false} {
		if got := sectionAnchor(target); got != want {
			t.Errorf("sectionAnchor(%q): got %v, want %v", target, got, want)
		}
	}
}

// The signature is what makes "the page stopped changing" decidable.
func TestSignatureChangesWithTheHandlesNotTheText(t *testing.T) {
	a := parseFixture(t, "signin.snapshot")
	b := parseFixture(t, "signin.snapshot")
	if a.signature() != b.signature() {
		t.Error("two reads of the same page must have the same signature")
	}
	b.Elements[0].Ref = "f2e99"
	if a.signature() == b.signature() {
		t.Error("a re-render that mints new refs must change the signature")
	}
}

// snapshotOf parses node lines the way they arrive: inside a capture's
// snapshot section.
func snapshotOf(lines ...string) Snapshot {
	return ParseSnapshot("### Snapshot\n```yaml\n" + strings.Join(lines, "\n") + "\n```")
}

// Playwright single-quotes a whole node key that would not read back as a yaml
// key - a name holding ": " is the common case, and every "pkg: change" title on
// a pull request list is one. A parser anchored on the role skipped all of them.
func TestParseSnapshotReadsQuotedNodeLines(t *testing.T) {
	snap := snapshotOf(
		`- 'link "flate: avoid FMA in EstimatedBits" [ref=e40] [cursor=pointer]':`,
		`  - /url: /golang/go/pull/81591`,
		`- 'textbox "Password: required" [ref=e8]': hunter2`,
		`- 'button "It''s done: really" [ref=e9]'`,
		`- 'link "Tags [3] #b {c}" [ref=e10]'`,
		`- link "Tags [4]" [ref=e11]`,
		`- link /api/ [ref=e12]`,
		`- 'button "Off: now" [disabled] [ref=e13]'`,
		`- link "Home" [ref=e14]`,
		`- link "Card [required" [ref=e15]`,
		`- link / [ref=e16]`,
	)
	want := []Element{
		{Ref: "e40", Role: "link", Label: "flate: avoid FMA in EstimatedBits"},
		{Ref: "e8", Role: "textbox", Label: "Password: required", State: "filled"},
		{Ref: "e9", Role: "button", Label: "It's done: really"},
		{Ref: "e10", Role: "link", Label: "Tags [3] #b {c}"},
		{Ref: "e11", Role: "link", Label: "Tags [4]"},
		{Ref: "e12", Role: "link", Label: "/api/"},
		{Ref: "e14", Role: "link", Label: "Home"},
		{Ref: "e15", Role: "link", Label: "Card [required"},
		{Ref: "e16", Role: "link", Label: "/"},
	}
	if len(snap.Elements) != len(want) {
		t.Fatalf("elements: got %d %+v, want %d", len(snap.Elements), snap.Elements, len(want))
	}
	for i, el := range want {
		if snap.Elements[i] != el {
			t.Errorf("element %d: got %+v, want %+v", i, snap.Elements[i], el)
		}
	}
}

func TestParseLineUndoesValueQuoting(t *testing.T) {
	cases := map[string]string{
		`- paragraph [ref=e1]: plain words`:       "plain words",
		`- paragraph [ref=e1]: "Price: 10 USD"`:   "Price: 10 USD",
		`- text: "tab\there \"quoted\" \\ back"`:  "tab\there \"quoted\" \\ back",
		`- 'heading "A: b" [level=2]': "c: d #e"`: "c: d #e",
		`- listitem: "- starts with a dash"`:      "- starts with a dash",
	}
	for line, want := range cases {
		n, ok := parseLine(line)
		if !ok {
			t.Errorf("%s: did not parse", line)
			continue
		}
		if n.Value != want {
			t.Errorf("%s: value %q, want %q", line, n.Value, want)
		}
	}
	for _, line := range []string{"```yaml", "- /url: https://example.com/?token=x", "- [Snapshot](./step2.yml)", "- 'link \"unterminated"} {
		if n, ok := parseLine(line); ok {
			t.Errorf("%q parsed as a node: %+v", line, n)
		}
	}
}

// A label that holds a field names the control it labels with the field's
// current value, so that control's name is the value in another node's clothes.
// These shapes are the live renderer's, for a wrapping label, aria-labelledby
// and `for=`; a search box's query repeated in a result further off is not one.
func TestParseSnapshotSealsNamesThatEchoAFieldValue(t *testing.T) {
	snap := snapshotOf(
		`- generic [ref=e1]:`,
		`  - generic [ref=e2]:`,
		`    - 'radio "Other: echo-secret-1" [ref=e3]'`,
		`    - text: "Other:"`,
		`    - textbox [ref=e4]: echo-secret-1`,
		`  - checkbox "Agree echo-secret-2" [ref=e5]`,
		`  - generic [ref=e6]:`,
		`    - text: Agree`,
		`    - 'textbox "Note: x" [ref=e7]':`,
		`      - /placeholder: Enter it`,
		`      - text: echo-secret-2`,
		`  - checkbox "Remember me" [ref=e8]`,
		`  - link "Home" [ref=e13]`,
		`  - textbox "Initial" [ref=e14]: e`,
		`  - searchbox "Search" [ref=e9]: shoes`,
		`  - list [ref=e10]:`,
		`    - listitem [ref=e11]:`,
		`      - link "Red shoes" [ref=e12]`,
	)
	for _, ref := range []string{"e3", "e5"} {
		if el, ok := snap.element(ref); ok {
			t.Errorf("a control named after a field's value was kept as an action: %+v", el)
		}
	}
	for _, ref := range []string{"e4", "e7", "e8", "e9", "e12", "e13", "e14"} {
		if _, ok := snap.element(ref); !ok {
			t.Errorf("%s was dropped, but its name echoes no field near it", ref)
		}
	}
	for _, line := range pageLines(snap.tree) {
		if strings.Contains(line, "echo-secret") {
			t.Errorf("a field's value reached a page line through another node's name: %q", line)
		}
	}
}

// A link's target only prunes the element its own node line produced: after a
// node line that does not parse, the element before it is someone else's.
func TestParseSnapshotPrunesOnlyTheAnchorLinkItself(t *testing.T) {
	snap := snapshotOf(
		`- button "Keep me" [ref=e1]`,
		`- link "x" [ref=e2] [bogus attr]:`,
		`  - /url: "#top"`,
	)
	if _, ok := snap.element("e1"); !ok {
		t.Errorf("an unrelated element was pruned: %+v", snap.Elements)
	}
}

// A modal leaves the background in the aria snapshot, but every click on it
// times out behind the overlay, so only the dialog's own controls are offered.
func TestOpenDialogHidesTheBackground(t *testing.T) {
	snap := ParseSnapshot(strings.Join([]string{
		"### Snapshot",
		"```yaml",
		`- generic [ref=f387e1]:`,
		`  - generic [ref=f387e2]:`,
		`    - dialog [active] [ref=f387e6295]:`,
		`      - button "Dismiss" [ref=f387e6296] [cursor=pointer]`,
		`      - contentinfo [ref=f387e6297]:`,
		`        - link "Privacy" [ref=f387e6298] [cursor=pointer]`,
		`  - navigation [ref=f387e3]:`,
		`    - link "Jobs" [ref=f387e4] [cursor=pointer]`,
		"```",
	}, "\n"))
	labels := make([]string, 0, len(snap.Elements))
	for _, el := range snap.Elements {
		labels = append(labels, el.Label)
	}
	if !slices.Equal(labels, []string{"Dismiss", "Privacy"}) {
		t.Errorf("elements: got %q, want only the dialog's controls", labels)
	}
	candidates, _ := actionSpace(snap, nil, nil)
	if !slices.ContainsFunc(candidates, func(c candidate) bool { return c.Label == "[dialog] button: Dismiss" }) {
		t.Error("the dialog's Dismiss button was not offered")
	}
}

// A dialog that holds no focus is not the one intercepting clicks - a
// non-modal panel, or one already dismissed - so the page stays on offer.
func TestInactiveDialogKeepsTheBackground(t *testing.T) {
	snap := ParseSnapshot(strings.Join([]string{
		"### Snapshot",
		"```yaml",
		`- dialog [ref=e1]:`,
		`  - button "Close" [ref=e2]`,
		`- link "Jobs" [ref=e3]`,
		"```",
	}, "\n"))
	if len(snap.Elements) != 2 {
		t.Errorf("elements: got %d, want the whole page", len(snap.Elements))
	}
}

// A site-wide search in the banner and a form's own box can share a role and
// read alike; the landmark each sits in is what tells the model which box a
// value belongs in, and a filled box says so without saying what it holds.
func TestTypingOptionsCarrySectionAndFilledState(t *testing.T) {
	snap := ParseSnapshot(strings.Join([]string{
		"### Snapshot",
		"```yaml",
		`- banner "Global Navigation" [ref=e1]:`,
		`  - combobox "I'm looking for..." [ref=e2]`,
		`  - navigation "Primary" [ref=e3]:`,
		`    - link "Jobs" [ref=e4] [cursor=pointer]`,
		`- main [ref=e5]:`,
		`  - generic [ref=e6]:`,
		`    - combobox "Describe the job you want" [ref=e7]: golang`,
		`    - combobox "City, state, or zip code" [expanded] [ref=e8]`,
		`    - checkbox "Remote" [checked] [ref=e9]`,
		`    - button "Search" [ref=e10]`,
		`- button "Chat" [ref=e11]`,
		"```",
	}, "\n"))
	want := []Element{
		{Ref: "e2", Role: "combobox", Label: "I'm looking for...", Section: "banner: Global Navigation"},
		{Ref: "e4", Role: "link", Label: "Jobs", Section: "navigation: Primary"},
		{Ref: "e7", Role: "combobox", Label: "Describe the job you want", Section: "main", State: "filled"},
		{Ref: "e8", Role: "combobox", Label: "City, state, or zip code", Section: "main", State: "expanded"},
		{Ref: "e9", Role: "checkbox", Label: "Remote", Section: "main", State: "checked"},
		{Ref: "e10", Role: "button", Label: "Search", Section: "main"},
		{Ref: "e11", Role: "button", Label: "Chat"},
	}
	if !slices.Equal(snap.Elements, want) {
		t.Fatalf("elements:\n got %+v\nwant %+v", snap.Elements, want)
	}
	candidates, _ := actionSpace(snap, []string{"location"}, nil)
	labels := map[string]string{}
	for _, c := range candidates {
		labels[c.Key] = c.Label
	}
	for key, label := range map[string]string{
		"type:e2:location": "type `values.location` into [banner: Global Navigation] combobox: I'm looking for...",
		"type:e7:location": "type `values.location` into [main] combobox: Describe the job you want (filled)",
		"type:e8:location": "type `values.location` into [main] combobox: City, state, or zip code",
		"e10":              "[main] button: Search",
		"e11":              "button: Chat",
	} {
		if labels[key] != label {
			t.Errorf("option %s: got %q, want %q", key, labels[key], label)
		}
	}
	for _, c := range candidates {
		if strings.Contains(c.Label, "golang") {
			t.Errorf("a field value reached an option: %q", c.Label)
		}
	}
	into := func(label string) []Step {
		return []Step{{URL: snap.URL, Action: "type `values.location` into " + label}}
	}
	if !typedHere(into("[main] combobox: City, state, or zip code"), snap.URL) {
		t.Error("typing into a sectioned combobox did not offer Enter")
	}
	if !typedHere(into("[main] textbox: City"), snap.URL) {
		t.Error("typing into a sectioned textbox did not offer Enter, though the focus stays in the field")
	}
}
