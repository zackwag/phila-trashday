// Package web serves the site: a page that finds an address's trash and
// recycling schedule, shows when to put the bins out, and links its
// calendar feed.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// contentSecurityPolicy allows only this server, plus the city's Carto API
// for address suggestions, which the page queries from the browser.
const contentSecurityPolicy = "default-src 'self'; connect-src 'self' https://phl.carto.com; " +
	"img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Handler serves the site's files. Fonts are cached for a year since they
// never change. Everything else is revalidated on every load, so a new
// release shows up right away.
func Handler() http.Handler {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		// Only possible if the embed directive above changes.
		panic(err)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/fonts/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
