// Package web embeds the KubeAura single-page UI so the whole platform
// ships as one self-contained binary.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var files embed.FS

// Handler serves the embedded static UI, falling back to index.html for the SPA.
func Handler() http.Handler {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	// http.FileServer serves index.html for "/" automatically and handles
	// static assets under /static.
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The UI is one embedded HTML file that changes on every rebuild;
		// without this, browsers heuristically cache it and keep showing a
		// stale UI after upgrades.
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
