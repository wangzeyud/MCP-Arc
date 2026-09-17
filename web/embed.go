// Package web embeds the compiled frontend (web/dist) into the Go binary so the
// admin console is served directly by MCP Arc — no separate static file server or
// dev proxy needed for end users.
package web

import "embed"

// DistFS holds the built Vue frontend output when it is available. The
// generated assets are added to this filesystem by the frontend build step.
//
//go:embed dist
var DistFS embed.FS
