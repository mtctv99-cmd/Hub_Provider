package web

import (
	"embed"
	"io/fs"
)

// StaticFS is embedded into the local core binary; no separate frontend server is required.
//
//go:embed index.html app.js style.css
var StaticFS embed.FS

func Files() fs.FS { return StaticFS }
