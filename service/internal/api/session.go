// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// SessionConfig backs the operator login flow.
//
// Deliberately the same shape as sc-component-backup's: one shared
// operator password, a session cookie carrying an HS256 JWT, no user
// accounts and no RBAC. Inventing a second auth story across SC
// Components would mean two things to get wrong and two things for an
// operator to learn (design decision D5).
type SessionConfig struct {
	// SigningKey signs session JWTs. Same key the plugin uses to mint
	// registration tokens, so it must be the RESOLVED secret value and
	// not a SecretsManager ARN — see secret_resolver.go.
	SigningKey []byte

	// OperatorPassword is the shared login secret. Empty means login is
	// disabled, and the session probe reports that so the SPA can say
	// "no password configured" rather than silently rejecting everyone.
	OperatorPassword string

	// CookieSecure should be false only for local HTTP development.
	CookieSecure bool

	// CookieTTL defaults to 12h.
	CookieTTL time.Duration
}

func (c *SessionConfig) cookieTTL() time.Duration {
	if c.CookieTTL == 0 {
		return 12 * time.Hour
	}
	return c.CookieTTL
}

// Session serves `GET /api/v1/session` — unauthenticated, so the SPA
// can decide what to render before it has a cookie.
//
// Reports whether a password is configured at all. Without that the
// SPA cannot distinguish "wrong password" from "this deployment has no
// password set", which is the difference between a typo and a
// misconfigured deploy.
func (c *SessionConfig) Session(w http.ResponseWriter, r *http.Request) {
	authed := false
	if raw := tokenFromRequest(r); raw != "" {
		authed = c.verify(raw)
	}
	writeJSON(w, http.StatusOK, map[string]bool{
		"authenticated": authed,
		"password_set":  c.OperatorPassword != "",
	})
}

// Login serves `POST /api/v1/login` with body `{"password": "..."}`.
func (c *SessionConfig) Login(w http.ResponseWriter, r *http.Request) {
	if c.OperatorPassword == "" {
		writeErr(w, http.StatusForbidden, "OBSERVATORY_OPERATOR_PASSWORD not set on the server")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password == "" {
		writeErr(w, http.StatusBadRequest, "missing password")
		return
	}
	// Constant-time compare: a byte-by-byte early return leaks the
	// shared secret through response timing. Same reasoning as Go's own
	// crypto/http2 and net/smtp.
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(c.OperatorPassword)) != 1 {
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	tok, err := c.mintOperatorJWT()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "mint failed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   c.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(c.cookieTTL()),
	})
	w.WriteHeader(http.StatusNoContent)
}

// Logout clears the cookie. Always 204 — a double logout is not an
// error.
func (c *SessionConfig) Logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   c.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (c *SessionConfig) mintOperatorJWT() (string, error) {
	now := time.Now()
	claims := &Claims{
		Op: OpOperator,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "sc-component-observatory",
			Subject:   "operator",
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(c.cookieTTL())),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(c.SigningKey)
}

func (c *SessionConfig) verify(raw string) bool {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return c.SigningKey, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	return err == nil && tok.Valid
}
