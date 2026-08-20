package serve

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
	got := candidateOrigins(st)
	want := []string{exampleOrigin, "https://sub.test.org"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidateOrigins=%v want %v", got, want)
	}
}

// A failed origin keeps whatever the prior snapshot held for it, so one unreadable
// tab never clears a login that is still valid.
func TestCarryForward(t *testing.T) {
	t.Parallel()
	prior := &cdp.StorageState{Origins: []cdp.Origin{
		{Origin: exampleOrigin, LocalStorage: []cdp.LocalStorageItem{{Name: "k", Value: "1"}}},
	}}
	got := carryForward(prior, &cdp.StorageState{}, []string{exampleOrigin})
	if len(got.Origins) != 1 || got.Origins[0].Origin != exampleOrigin {
		t.Fatalf("failed origin should keep its prior localStorage, got %+v", got.Origins)
	}
	if carryForward(nil, &cdp.StorageState{}, []string{exampleOrigin}).Origins != nil {
		t.Fatal("a nil prior has nothing to carry forward")
	}
}
