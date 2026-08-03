package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedShellServesWithOrWithoutExplicitWebBuild(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("shell response = %d %q", rec.Code, rec.Body.String())
	}
	if !Built() && !strings.Contains(rec.Body.String(), "web UI was not built") {
		t.Fatalf("stub did not explain the missing explicit build: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestHistoryRoutesAndTraversalFallBackToShell(t *testing.T) {
	for _, path := range []string{"/a/t-123", "/a/t-123/diff", "/../embed.go", "/assets/../../embed.go"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "package webui") {
			t.Fatalf("GET %s = %d %q", path, rec.Code, rec.Body.String())
		}
	}
}
