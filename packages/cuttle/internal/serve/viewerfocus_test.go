package serve

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestViewerFocusRejectsRemotePeer(t *testing.T) {
	m := &multiplexer{}
	req := httptest.NewRequest(http.MethodGet, "http://localhost:9222/viewer/focus", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	response := httptest.NewRecorder()
	m.handleViewerFocus(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d", response.Code)
	}
}
