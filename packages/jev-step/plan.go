package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
)

var errPlan = errors.New("plan")

// plan is what the LLM writes once, before the loop starts. Jev never writes
// any of it: every value typed into the page comes from here verbatim.
type plan struct {
	Steps []planStep `json:"steps"`
}

// planStep is one milestone of the plan. Fill holds the values this step may
// type, keyed by a name; only the NAMES are ever sent to Jev, which is what
// keeps credentials and `{{cuttle:NAME}}` sentinels out of the decision call.
type planStep struct {
	Description string            `json:"description"`
	Expect      string            `json:"expect"`
	Fill        map[string]string `json:"fill"`
}

// fillNames returns the step's fill keys in a stable order, so a rerun of the
// same page sends Jev the same option list.
func (s planStep) fillNames() []string {
	names := make([]string, 0, len(s.Fill))
	for name := range s.Fill {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func loadPlan(path string) (plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return plan{}, fmt.Errorf("read plan: %w", err)
	}
	var p plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return plan{}, fmt.Errorf("parse plan: %w", err)
	}
	if len(p.Steps) == 0 {
		return plan{}, fmt.Errorf("%w: %s has no steps", errPlan, path)
	}
	return p, nil
}
