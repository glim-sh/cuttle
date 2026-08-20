package profile

import (
	"slices"
	"testing"

	"github.com/glim-sh/cuttle/internal/cdp"
)

const exampleOrigin = "https://example.com"

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
	got := CandidateOrigins(st)
	want := []string{exampleOrigin, "https://sub.test.org"}
	if !slices.Equal(got, want) {
		t.Fatalf("CandidateOrigins=%v want %v", got, want)
	}
}
