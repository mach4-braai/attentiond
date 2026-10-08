// Package ui embeds the single page attentiond serves at GET /: plain HTML,
// JavaScript and CSS, with no build step and no request that leaves the
// machine.
package ui

import (
	"embed"
	"net/http"
)

//go:embed index.html app.js style.css
var files embed.FS

// Handler serves the page and its two assets. Any other path is a 404, so a
// mistyped API route does not answer with HTML.
func Handler() http.Handler {
	return http.FileServerFS(files)
}
