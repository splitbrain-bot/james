// Package web holds the files the server delivers to the browser.
package web

import "embed"

// Files holds the popup page, the widget script and the static assets.
//
//go:embed index.html james.js static
var Files embed.FS
