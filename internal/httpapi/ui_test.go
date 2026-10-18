package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The page is the way into the daemon, so it has to load from the same
// handler that serves the API, with the types a browser needs to run it.
func TestServesThePage(t *testing.T) {
	handler, _, _ := newTestServer(t)

	for _, tc := range []struct {
		path        string
		contentType string
		contains    string
	}{
		{"/", "text/html", "<title>attentiond</title>"},
		{"/app.js", "javascript", "/api/attention"},
		{"/style.css", "text/css", "--ready"},
	} {
		recorder := do(t, handler, http.MethodGet, tc.path, "")
		if recorder.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", tc.path, recorder.Code)
			continue
		}
		if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, tc.contentType) {
			t.Errorf("GET %s content type = %q, want it to contain %q", tc.path, got, tc.contentType)
		}
		if !strings.Contains(recorder.Body.String(), tc.contains) {
			t.Errorf("GET %s body does not contain %q", tc.path, tc.contains)
		}
	}

	if recorder := do(t, handler, http.MethodGet, "/api/nothing", ""); recorder.Code != http.StatusNotFound {
		t.Errorf("GET /api/nothing = %d, want 404", recorder.Code)
	}
}
