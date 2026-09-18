package jev

import (
	"os"
	"path/filepath"
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
		{Ref: "f2e3", Role: "link", Label: "Home"},
		{Ref: "f2e4", Role: "link", Label: "Cart (0)"},
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
	if _, ok := snap.element("f1e3"); !ok {
		t.Error("the sidebar links the session recovered through were not parsed")
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
