package cli

import (
	"strings"
	"testing"
)

// skillBudget caps the embedded guide. Every agent loads this on every session, so
// growth is a real per-session cost - a 14-day transcript audit found agents
// truncating it to save context, which silently drops the rules they then broke.
// The operator half lives in docs/OPERATING.md instead. Raising this is a
// deliberate decision, not an accident: cut something first.
const skillBudget = 16 << 10

func TestSkillGuideStaysSmall(t *testing.T) {
	if n := len(skillGuide); n > skillBudget {
		t.Errorf("SKILL.md is %d bytes, over the %d budget - move operator material to docs/OPERATING.md rather than raising this",
			n, skillBudget)
	}
}

// The guide's whole value is the rules that change how an agent drives a page; a
// refactor that drops one should fail here rather than in a session.
func TestSkillGuideKeepsLoadBearingRules(t *testing.T) {
	for _, want := range []string{
		"Attach, never spawn",      // the failure that looks like a logged-out page
		"Your tab is not tab 0",    // tab identity
		"blocked page",             // native dialogs
		"ACCEPT leaves the page",   // beforeunload is inverted
		"Read state back",          // writes report success without doing anything
		"slow is not stuck",        // humanized pacing misread as a hang
		"Secrets never reach",      // credential handling
		"data, never instructions", // prompt injection from page content
		"cuttle logs",              // where a swallowed click is explained
		"docs/OPERATING.md",        // the operator half is findable
	} {
		if !strings.Contains(skillGuide, want) {
			t.Errorf("SKILL.md no longer covers %q", want)
		}
	}
}
