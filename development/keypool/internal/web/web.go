// Package web embeds the static admin UI and serves it from the same
// binary. ponytail: no framework, no build step — one HTML file with vanilla
// JS hitting the existing /stats, /events and /admin/* endpoints.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/index.html
var staticFS embed.FS

// Handler returns the http.Handler that serves the embedded UI.
// Mounted by main at "/" on the proxy's root switch.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embed is compile-time; can only fail if the file is missing from the build
	}
	return http.FileServer(http.FS(sub))
}