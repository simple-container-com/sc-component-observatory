// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package ui embeds the built React SPA so the whole dashboard ships
// as one binary — no sidecar, no CDN, no separate static bucket to
// provision or keep in sync with the API it talks to.
package ui

import (
	"embed"
	"io/fs"
)

// BuildFS holds the compiled SPA.
//
// `all:` matters: the default embed pattern silently skips files whose
// names begin with `_` or `.`, and bundlers emit exactly those. Without
// it an asset can vanish from the binary with no build error — the
// failure then shows up as a blank page in production.
//
//go:embed all:dist
var BuildFS embed.FS

// Dist returns the SPA rooted at its index, or an error when the UI
// was never built.
//
// Returning an error rather than panicking lets the server start and
// serve its API with a warning: a Go-only change should not be blocked
// on having run npm, and an operator debugging the API does not need
// the UI present.
func Dist() (fs.FS, error) {
	return fs.Sub(BuildFS, "dist")
}
