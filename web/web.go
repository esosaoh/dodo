// Package web embeds the static frontend served by the API server.
package web

import "embed"

//go:embed index.html style.css app.js
var Files embed.FS
