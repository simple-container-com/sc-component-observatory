// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package ui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Handler serves the SPA with history-API fallback.
//
// A React router owns client-side paths like /services/forge-storage--staging,
// which do not exist as files. A plain http.FileServer answers those
// with 404, so the app works on a fresh load of `/` and breaks on a
// refresh or a shared deep link — a bug that only shows up once
// somebody actually uses the UI.
//
// So: serve the file when it exists, and fall back to index.html for
// anything else that looks like a navigation. Requests under /assets/
// are NOT given the fallback — a missing bundle should surface as a 404
// rather than as index.html served with a JavaScript content-type,
// which produces a far more confusing browser error.
func Handler(dist fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if upath == "" {
			upath = "index.html"
		}

		if f, err := dist.Open(upath); err == nil {
			_ = f.Close()
			// Hashed bundles are immutable; index.html must not be
			// cached or a deploy leaves browsers on the old app.
			if strings.HasPrefix(upath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(upath, "assets/") {
			http.NotFound(w, r)
			return
		}

		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "SPA not built", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
