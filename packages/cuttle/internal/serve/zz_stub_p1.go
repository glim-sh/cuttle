package serve

import "regexp"

// STUB(P1): E1 provides this validator at integration.
func validBoundSeed(name string) bool {
	return name != "state" && name != "logs" && regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`).MatchString(name)
}
