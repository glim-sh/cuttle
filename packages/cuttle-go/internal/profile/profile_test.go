package profile

import (
	"slices"
	"testing"

	"github.com/glim-sh/cuttle/packages/cuttle-go/internal/cdp"
)

const (
	exampleDomain = "example.com"
	exampleOrigin = "https://example.com"
)

func TestValidName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"default", true},
		{"linkedin", true},
		{"a_b-C9", true},
		{"", false},
		{"__default__", false},
		{"has space", false},
		{"dots.bad", false},
		{"slash/bad", false},
	}
	for _, tt := range tests {
		if got := ValidName(tt.name); got != tt.want {
			t.Errorf("ValidName(%q)=%v want %v", tt.name, got, tt.want)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := &cdp.StorageState{
		Cookies: []cdp.Cookie{{Name: "sid", Value: "v", Domain: exampleDomain, Path: "/", Expires: -1}},
		Origins: []cdp.Origin{{Origin: exampleOrigin, LocalStorage: []cdp.LocalStorageItem{{Name: "k", Value: "1"}}}},
	}
	if err := saveState(dir, want); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got, err := loadState(dir)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if len(got.Cookies) != 1 || got.Cookies[0].Name != "sid" {
		t.Fatalf("cookies: %+v", got.Cookies)
	}
	if len(got.Origins) != 1 || got.Origins[0].Origin != exampleOrigin {
		t.Fatalf("origins: %+v", got.Origins)
	}
}

func TestLoadStateMissing(t *testing.T) {
	t.Parallel()
	st, err := loadState(t.TempDir())
	if err != nil {
		t.Fatalf("missing state should not error: %v", err)
	}
	if len(st.Cookies) != 0 || len(st.Origins) != 0 {
		t.Fatalf("expected empty state, got %+v", st)
	}
}

func TestCandidateOrigins(t *testing.T) {
	t.Parallel()
	st := &cdp.StorageState{
		Cookies: []cdp.Cookie{
			{Name: "a", Domain: ".example.com"},
			{Name: "b", Domain: "sub.test.org"},
			{Name: "c", Domain: ".example.com"}, // duplicate origin
		},
		Origins: []cdp.Origin{{Origin: exampleOrigin}},
	}
	got := candidateOrigins(st)
	want := []string{exampleOrigin, "https://sub.test.org"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidateOrigins=%v want %v", got, want)
	}
}
