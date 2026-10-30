package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	tests := []struct {
		path        string
		status      int
		contentType string
		cache       string
		contains    string
	}{
		{"/", http.StatusOK, "text/html", "no-cache", "When&rsquo;s trash night?"},
		{"/app.js", http.StatusOK, "text/javascript", "no-cache", "normalizeAddress"},
		{"/style.css", http.StatusOK, "text/css", "no-cache", "--bin-blue"},
		{"/favicon.svg", http.StatusOK, "image/svg+xml", "no-cache", "<svg"},
		{"/fonts/overpass-latin-wght.woff2", http.StatusOK, "font/woff2", "public, max-age=31536000, immutable", ""},
		// FileServer drops caching headers from its error responses.
		{"/missing.html", http.StatusNotFound, "", "", ""},
	}
	h := Handler()
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d", rec.Code, tt.status)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tt.contentType) {
				t.Errorf("Content-Type = %q, want %q", ct, tt.contentType)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != tt.cache {
				t.Errorf("Cache-Control = %q, want %q", cc, tt.cache)
			}
			if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "connect-src 'self' https://phl.carto.com") {
				t.Errorf("Content-Security-Policy = %q", rec.Header().Get("Content-Security-Policy"))
			}
			if !strings.Contains(rec.Body.String(), tt.contains) {
				t.Errorf("body doesn't contain %q", tt.contains)
			}
		})
	}
}
