package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writePlan(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

func TestLoadPlan(t *testing.T) {
	path := writePlan(t, `{
	  "steps": [
	    {
	      "description": "sign in with the test account",
	      "expect": "the account menu is visible",
	      "fill": {"username": "demo-user", "password": "{{cuttle:DEMO_PASS}}"}
	    },
	    {"description": "open the orders page", "expect": "the orders table is visible"}
	  ]
	}`)

	p, err := loadPlan(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(p.Steps))
	}
	// The sentinel has to survive byte-for-byte: cuttle substitutes it inside its
	// own CDP frame, and anything that rewrites it types a literal into the field.
	if got := p.Steps[0].Fill["password"]; got != "{{cuttle:DEMO_PASS}}" {
		t.Errorf("secret sentinel: got %q", got)
	}
	if got, want := p.Steps[0].fillNames(), []string{"password", "username"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("fill names: got %v, want %v", got, want)
	}
	if len(p.Steps[1].fillNames()) != 0 {
		t.Error("a step with no fill map must offer no names")
	}
}

func TestLoadPlanRejectsAnEmptyPlan(t *testing.T) {
	if _, err := loadPlan(writePlan(t, `{"steps": []}`)); !errors.Is(err, errPlan) {
		t.Errorf("got %v, want an errPlan", err)
	}
}
