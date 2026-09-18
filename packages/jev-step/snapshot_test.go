package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fixtures are byte-for-byte `playwright-cli snapshot` output (v0.1.20,
// chrome-for-testing) captured against a local page built to carry the shapes
// that broke earlier drafts of the parser: frame-qualified refs, a disabled
// control, an escaped quote inside an accessible name, a div given role=button,
// and the error-plus-modal output a pending dialog produces.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(raw)
}

func TestParseSnapshot(t *testing.T) {
	cases := map[string]struct {
		fixture  string
		url      string
		title    string
		modal    string
		elements []element
	}{
		"sign-in page": {
			fixture: "signin.snapshot",
			url:     "http://127.0.0.1:8799/",
			title:   "Cuttle demo shop",
			elements: []element{
				{ID: "f2e3", Role: "link", Label: "Home"},
				{ID: "f2e4", Role: "link", Label: "Cart (0)"},
				{ID: "f2e7", Role: "textbox", Label: "Username"},
				{ID: "f2e8", Role: "textbox", Label: "Password"},
				{ID: "f2e9", Role: "combobox", Label: ""},
				{ID: "f2e10", Role: "checkbox", Label: "Remember me"},
				{ID: "f2e11", Role: "button", Label: "Login"},
				{ID: "f2e12", Role: "button", Label: "Help"},
			},
		},
		"checkout page": {
			fixture: "checkout.snapshot",
			url:     "http://127.0.0.1:8799/checkout.html",
			title:   "Checkout",
			elements: []element{
				{ID: "f1e3", Role: "link", Label: "Home"},
				{ID: "f1e6", Role: "textbox", Label: "Postal code"},
				{ID: "f1e7", Role: "textbox", Label: "Delivery notes"},
				{ID: "f1e8", Role: "radio", Label: "Standard"},
				{ID: "f1e9", Role: "radio", Label: "Express"},
				// f1e10, the disabled Continue button, is deliberately absent.
				{ID: "f1e11", Role: "button", Label: "Cancel"},
				{ID: "f1e12", Role: "link", Label: `Help "centre"`},
				{ID: "f1e13", Role: "searchbox", Label: "Find a product"},
				{ID: "f1e14", Role: "button", Label: "Custom widget"},
			},
		},
		"pending dialog": {
			fixture: "modal.snapshot",
			modal:   `["alert" dialog with message "nope"]: can be handled by dialog-accept or dialog-dismiss`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			snap := parseSnapshot(readFixture(t, tc.fixture))
			if snap.URL != tc.url || snap.Title != tc.title {
				t.Errorf("page identity: got %q / %q, want %q / %q", snap.URL, snap.Title, tc.url, tc.title)
			}
			if snap.Modal != tc.modal {
				t.Errorf("modal: got %q, want %q", snap.Modal, tc.modal)
			}
			if len(snap.Elements) != len(tc.elements) {
				t.Fatalf("got %d elements, want %d: %+v", len(snap.Elements), len(tc.elements), snap.Elements)
			}
			for i, want := range tc.elements {
				if snap.Elements[i] != want {
					t.Errorf("element %d: got %+v, want %+v", i, snap.Elements[i], want)
				}
			}
		})
	}
}

// The parser is also the privacy boundary: whatever it does not extract can
// never be sent. Page prose lives in roles this drops entirely.
func TestParseSnapshotDropsPageText(t *testing.T) {
	snap := parseSnapshot(readFixture(t, "signin.snapshot"))
	for _, el := range snap.Elements {
		if strings.Contains(el.Label, "must never be sent") {
			t.Fatalf("page body text reached an element: %+v", el)
		}
	}
}

func TestParseSnapshotCapsAtJevsLimit(t *testing.T) {
	var b strings.Builder
	b.WriteString("### Snapshot\n")
	for i := range 400 {
		b.WriteString(`  - button "Row" [ref=e` + strconv.Itoa(i) + "]\n")
	}
	if got := len(parseSnapshot(b.String()).Elements); got != maxElements {
		t.Errorf("got %d elements, want the %d Jev accepts", got, maxElements)
	}
}
