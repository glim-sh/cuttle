package serve

import (
	"context"
	"fmt"
	"time"
)

// STUB(A2): compatibility for the old capture, wait and sentinel tools.
// Delete this file, the embedded legacyTabState and legacyFreshTab call when A2 lands.
type (
	capturedTab    struct{ at time.Time }
	legacyTabState struct {
		owned    map[string]bool
		current  string
		captured map[string]capturedTab
	}
)

func (s *agentSession) adopt(_ []pageInfo) {}
func (a *aggregator) legacyFreshTab(ctx context.Context, s *agentSession) {
	pages, _ := a.browser.pages(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owned = map[string]bool{}
	s.captured = map[string]capturedTab{}
	if len(pages) > 0 {
		s.current = pages[len(pages)-1].ID
	}
}

// screenshotRefusal holds screenshots of a tab a value was just captured from
// for the whole hold: an image cannot be masked, and a URL change (a fragment,
// history.replaceState) does not mean the value left the screen.
func (a *aggregator) screenshotRefusal(s *agentSession) string {
	s.mu.Lock()
	c, ok := s.captured[s.current]
	s.mu.Unlock()
	left := screenshotHold - time.Since(c.at)
	if !ok || left <= 0 {
		return ""
	}
	return fmt.Sprintf("cuttle: a secret was captured from this tab %ds ago, so screenshots of it are refused for another %ds"+
		" - an image cannot be masked. Use browser_snapshot, whose text is masked, or wait.",
		int(time.Since(c.at).Seconds()), int(left.Seconds())+1)
}

func (b cdpBrowser) activate(ctx context.Context, pageID string) {
	ctx, cancel := context.WithTimeout(ctx, browserCallTimeout)
	defer cancel()
	port, err := b.port()
	if err != nil {
		return
	}
	conn, err := dialBrowser(ctx, port)
	if err != nil {
		return
	}
	defer conn.close()
	_, _ = conn.call(ctx, "", "Target.activateTarget", map[string]any{keyTargetID: pageID})
}
