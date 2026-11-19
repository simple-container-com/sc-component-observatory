// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import "strings"

// secretKeyHints are substrings that mark a config key as sensitive.
//
// Matched case-insensitively against the KEY, not the value. Matching
// values would be worse: a value-based heuristic either misses a
// secret that does not look like one, or redacts an innocent string
// that happens to resemble a token — and the first failure mode means
// a credential lands in object storage and is then served to a
// browser.
//
// The list is deliberately broad. A false positive costs an operator
// one redacted field in a read-only view; a false negative leaks a
// credential, so the asymmetry justifies over-redacting.
var secretKeyHints = []string{
	"secret", "password", "passwd", "token", "apikey", "api_key",
	"privatekey", "private_key", "credential", "signingkey",
	"signing_key", "accesskey", "access_key", "sessionkey",
	"passphrase", "auth", "cert", "key",
}

// SanitizeClientConfig returns a deep copy of a consumer's client
// descriptor with sensitive values replaced by a marker.
//
// Called before the config is sent to the observatory service, because
// it lands in object storage and is served to the dashboard — so the
// stripping has to happen at the source, not at render time where a
// future endpoint could forget it.
//
// What survives: everything structural — the template, compute shape,
// scaling bounds, schedules, the `uses:` list, healthcheck settings.
// That is the whole reason to keep the config at all.
//
// What does not: anything whose key looks like a credential, and
// anything that still carries an unresolved `${secret:...}` reference
// (harmless in itself, but it reveals the operator's secret ALIASES,
// which is a needless disclosure for a view that cannot use them).
func SanitizeClientConfig(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	return sanitizeMap(in)
}

const redacted = "[redacted]"

func sanitizeMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isSecretKey(k) {
			// Record that the key EXISTED. An operator debugging a
			// deploy needs to know a signingKey was configured; they do
			// not need its value, and omitting the key entirely would
			// make "unset" and "set but hidden" indistinguishable.
			out[k] = redacted
			continue
		}
		out[k] = sanitizeValue(v)
	}
	return out
}

func sanitizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return sanitizeMap(t)
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, sanitizeValue(item))
		}
		return out
	case string:
		if strings.Contains(t, "${secret:") {
			return redacted
		}
		// A credential embedded in a URI survives key-matching, because
		// the key that holds it is innocuous: MONGO_URI, DATABASE_URL,
		// AMQP_ADDR. Caught by a test that scanned the whole serialized
		// tree rather than the paths this function remembered to handle.
		if hasURICredential(t) {
			return redacted
		}
		return t
	default:
		return v
	}
}

// hasURICredential reports whether a string looks like a URI carrying
// userinfo credentials — `scheme://user:password@host`.
//
// Deliberately narrow: it requires a scheme separator, an "@" after
// it, and a ":" inside the userinfo segment. That matches
// mongodb+srv://u:p@host and postgres://u:p@host while leaving
// https://example.com/path and plain "host:port" strings alone, so
// ordinary config values are not needlessly destroyed.
func hasURICredential(v string) bool {
	i := strings.Index(v, "://")
	if i < 0 {
		return false
	}
	rest := v[i+3:]
	at := strings.Index(rest, "@")
	if at <= 0 {
		return false
	}
	userinfo := rest[:at]
	// A "/" before the "@" means the "@" is in the path, not userinfo.
	if strings.Contains(userinfo, "/") {
		return false
	}
	return strings.Contains(userinfo, ":")
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, hint := range secretKeyHints {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

// HealthPathFromClientConfig digs a service's own health path out of
// its client config.
//
// A service knows where its probe lives better than any fleet-wide
// default does, and several shapes express it. Checked in order of
// explicitness:
//
//  1. `healthPath` / `healthcheck.path` — an explicit statement.
//  2. A compose service's `healthcheck.test`, when it is an HTTP probe
//     (`curl`/`wget` against a URL) — parsed for the path only.
//
// Returns "" when nothing is declared, and the caller falls back to
// the component-wide default. Deliberately does NOT guess from a port
// or a route list: a wrong health path reports a healthy service as
// down, which is worse than probing a sensible default.
func HealthPathFromClientConfig(cfg map[string]any) string {
	if cfg == nil {
		return ""
	}
	if p, ok := cfg["healthPath"].(string); ok && p != "" {
		return normalizePath(p)
	}
	if hc, ok := cfg["healthcheck"].(map[string]any); ok {
		if p, ok := hc["path"].(string); ok && p != "" {
			return normalizePath(p)
		}
		if p := pathFromComposeTest(hc["test"]); p != "" {
			return p
		}
	}
	// A compose stack nests healthchecks per service; take the first
	// one that declares an HTTP probe.
	if svcs, ok := cfg["services"].(map[string]any); ok {
		for _, raw := range svcs {
			svc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			hc, ok := svc["healthcheck"].(map[string]any)
			if !ok {
				continue
			}
			if p, ok := hc["path"].(string); ok && p != "" {
				return normalizePath(p)
			}
			if p := pathFromComposeTest(hc["test"]); p != "" {
				return p
			}
		}
	}
	return ""
}

// pathFromComposeTest extracts a URL path from a compose healthcheck
// `test`, which is either a string or a []any command vector.
func pathFromComposeTest(test any) string {
	var parts []string
	switch t := test.(type) {
	case string:
		parts = strings.Fields(t)
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				parts = append(parts, strings.Fields(s)...)
			}
		}
	default:
		return ""
	}
	for _, p := range parts {
		// Only treat it as a probe URL if it really looks like one;
		// a bare word could be a flag or a binary name.
		if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
			continue
		}
		rest := p
		for _, scheme := range []string{"http://", "https://"} {
			rest = strings.TrimPrefix(rest, scheme)
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			return normalizePath(rest[i:])
		}
		// A URL with no path at all means the root.
		return "/"
	}
	return ""
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// EndpointURLFromClientConfig digs a consumer's own public base URL out
// of its client config.
//
// Without this the registry records no endpoint, the health prober has
// nothing to probe, and every tenant sits at `health: unknown` forever
// — which is how the first real tenant registered (forge-storage,
// 2026-11-19: a correct record with `endpoint_url: ""`).
//
// `config.domain` is the right source and is template-GENERIC: every SC
// client stack that is reachable declares it, whatever it deploys to —
// a Lambda behind a Function URL, an ECS service behind an ALB, a k8s
// ingress. Deriving from the cloud's own address instead (a Function
// URL, a load-balancer hostname) would be both cloud-specific and
// wrong, since the operator's DNS name is the address the service is
// actually served on.
//
// Checked in order:
//
//  1. `endpointUrl` / `publicUrl` — an explicit statement, for a
//     service whose public address is not its `domain`.
//  2. `domain` — the normal case.
//  3. a compose stack's per-service `domain`, first one that has it.
//
// Returns "" when nothing is declared, which the prober records as
// `unknown` rather than `down`: a service with no published endpoint is
// a wiring gap, not an outage, and the two must stay distinguishable.
func EndpointURLFromClientConfig(cfg map[string]any) string {
	if cfg == nil {
		return ""
	}
	if u := firstStringKey(cfg, "endpointUrl", "publicUrl", "domain"); u != "" {
		return normalizeBaseURL(u)
	}
	if svcs, ok := cfg["services"].(map[string]any); ok {
		for _, raw := range svcs {
			svc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if u := firstStringKey(svc, "endpointUrl", "publicUrl", "domain"); u != "" {
				return normalizeBaseURL(u)
			}
		}
	}
	return ""
}

func firstStringKey(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// normalizeBaseURL turns a bare hostname into an https base and strips
// a trailing slash.
//
// https, not http: an SC-deployed service is always behind TLS (a
// Function URL, a registrar-managed record, an ingress), and probing
// http would either redirect — which the prober counts as UP, so the
// reading would be of the redirect rather than the service — or fail
// outright.
func normalizeBaseURL(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://" + s
}
