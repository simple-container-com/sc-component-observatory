// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// writeJSON emits a JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr emits `{"error": "..."}`.
//
// Always JSON, never bare text: the first SC Component served HTML 404s
// to a JSON-expecting SPA, whose error handler stringified the markup
// and produced an unreadable crash. A client that gets a 4xx/5xx here
// can always parse the reason.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// pathParam reads a Go 1.22+ ServeMux wildcard.
//
// Wrapped so the handlers don't bind to a specific router: the first
// Component used chi, and swapping it meant touching every handler.
func pathParam(r *http.Request, name string) string {
	return r.PathValue(name)
}

// claimsKey is private so no caller outside this package can fabricate
// claims into a request context.
type claimsKey struct{}

// Claims is the JWT shape both the plugin (minting registration
// tokens) and the operator login flow produce.
type Claims struct {
	// Op is the token's purpose:
	//
	//	"register"  a plugin push-registering one tenant
	//	"operator"  a logged-in human
	//	"scheduler" the deploy template's own timer, firing the sweep
	//
	// Checked per route, not just for validity. Every tenant's plugin
	// holds a "register" token, so a route that accepted any valid
	// token would let any consumer stack drive the whole fleet's
	// healthcheck sweep.
	Op string `json:"op"`

	// Tenant scopes a registration token to exactly one stack.
	Tenant string `json:"tenant"`

	jwt.RegisteredClaims
}

// SessionCookieName is the operator session cookie.
//
// Namespaced to this component: observatory is a reusable SC Component,
// so a deployment sitting on the same origin as another sc-component-*
// service must not collide with its cookie.
const SessionCookieName = "sc_observatory_session"

// JWTMiddleware verifies an HS256 token from either the
// `Authorization: Bearer` header or the session cookie, and puts the
// claims in the request context.
//
// Rejects with 401 on: no credential, wrong algorithm, bad signature,
// expired or not-yet-valid.
func JWTMiddleware(signingKey []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := tokenFromRequest(r)
			if raw == "" {
				writeErr(w, http.StatusUnauthorized, "missing or malformed credentials")
				return
			}
			claims := &Claims{}
			tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, jwt.ErrSignatureInvalid
				}
				return signingKey, nil
			}, jwt.WithValidMethods([]string{"HS256"}))
			if err != nil || !tok.Valid {
				writeErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey{}, claims)))
		})
	}
}

// tokenFromRequest prefers the Bearer header so a curl against a
// browser-authed deployment can still override.
func tokenFromRequest(r *http.Request) string {
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimPrefix(authz, "Bearer ")
	}
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// ClaimsFromContext returns the verified claims, or nil when the
// middleware did not run.
func ClaimsFromContext(ctx context.Context) *Claims {
	if v, ok := ctx.Value(claimsKey{}).(*Claims); ok {
		return v
	}
	return nil
}

// Token purposes. Constants rather than literals because they are
// compared in two repos — the service checks them, the plugin mints
// them — and a typo on either side fails as a 401 with nothing naming
// the mismatch.
const (
	OpRegister  = "register"
	OpOperator  = "operator"
	OpScheduler = "scheduler"
)
