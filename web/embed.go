// Package web embeds the browser app so the server is a single binary.
package web

import "embed"

//go:embed index.html style.css manifest.webmanifest sw.js icon.svg generic.svg js lib
var FS embed.FS
