// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

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
//
// `fpKey` keys the fingerprint appended to each redaction marker — see
// redact. Pass the component's own signingKey. An empty key degrades to
// a bare `[redacted]`, which is safe but makes rotations invisible to
// the diff.
func SanitizeClientConfig(in map[string]any, fpKey []byte) map[string]any {
	if in == nil {
		return nil
	}
	return sanitizeMap(in, fpKey)
}

const redactedPlain = "[redacted]"

// redact replaces a sensitive value with a marker carrying a short
// KEYED fingerprint of the value: `[redacted:ab12cd]`.
//
// The fingerprint exists for the deploy-config diff. With a constant
// marker, a ROTATED credential diffs as unchanged — the single most
// important config change a service can undergo renders as "nothing
// happened", which is precisely the silent degradation this component
// is supposed to eliminate. With a fingerprint it renders as "this
// secret changed", and the diff never shows the value.
//
// Keyed with HMAC, not a plain hash: secrets are often short or
// low-entropy (a password, a 6-digit key id), and an unkeyed digest
// published to a dashboard would be offline-guessable. The key is the
// component's signingKey, which lives in the operator's SC registry and
// never in the bucket the fingerprints land in.
//
// Six hex characters: enough that two different values colliding is
// unlikely (~1 in 16M), short enough to be visibly a fingerprint rather
// than something to copy around.
func redact(value string, fpKey []byte) string {
	if len(fpKey) == 0 {
		return redactedPlain
	}
	mac := hmac.New(sha256.New, fpKey)
	mac.Write([]byte(value))
	return "[redacted:" + hex.EncodeToString(mac.Sum(nil))[:6] + "]"
}

// redactAny fingerprints a non-string secret (a nested map or list
// under a credential-shaped key) off its canonical JSON, so a changed
// secret BLOCK is as visible as a changed secret string.
func redactAny(v any, fpKey []byte) string {
	if s, ok := v.(string); ok {
		return redact(s, fpKey)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		// Unfingerprintable, so say nothing about it rather than
		// fingerprint a Go-formatted approximation that could differ
		// between runs and invent a change on every deploy.
		return redactedPlain
	}
	return redact(string(raw), fpKey)
}

func sanitizeMap(in map[string]any, fpKey []byte) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isSecretKey(k) {
			// Record that the key EXISTED. An operator debugging a
			// deploy needs to know a signingKey was configured; they do
			// not need its value, and omitting the key entirely would
			// make "unset" and "set but hidden" indistinguishable.
			out[k] = redactAny(v, fpKey)
			continue
		}
		out[k] = sanitizeValue(v, fpKey)
	}
	return out
}

func sanitizeValue(v any, fpKey []byte) any {
	switch t := v.(type) {
	case map[string]any:
		return sanitizeMap(t, fpKey)
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, sanitizeValue(item, fpKey))
		}
		return out
	case string:
		if strings.Contains(t, "${secret:") {
			return redact(t, fpKey)
		}
		// A credential embedded in a URI survives key-matching, because
		// the key that holds it is innocuous: MONGO_URI, DATABASE_URL,
		// AMQP_ADDR. Caught by a test that scanned the whole serialized
		// tree rather than the paths this function remembered to handle.
		if hasURICredential(t) {
			return redact(t, fpKey)
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
	// Searched the same nested way as the endpoint URL, and for the same
	// reason: the descriptor is wrapped, so a top-level lookup finds
	// nothing. See findStringDeep.
	if p := findStringDeep(cfg, "healthPath"); p != "" {
		return normalizePath(p)
	}
	// A compose stack nests healthchecks per service; findMapDeep takes
	// the shallowest `healthcheck` block, whichever level it sits at.
	if hc := findMapDeep(cfg, "healthcheck"); hc != nil {
		if p, ok := hc["path"].(string); ok && p != "" {
			return normalizePath(p)
		}
		if p := pathFromComposeTest(hc["test"]); p != "" {
			return p
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
	if u := findStringDeep(cfg, "endpointUrl", "publicUrl", "domain"); u != "" {
		return normalizeBaseURL(u)
	}
	return ""
}

// maxDescriptorDepth bounds the nested search below.
//
// The descriptor's own nesting is two levels (`Config` → `config`), and
// a compose stack adds two more (`services` → `<name>`). Four covers
// every real shape with one to spare, while keeping the search from
// wandering into an arbitrarily deep env or policy blob where a `domain`
// key would mean something else entirely.
const maxDescriptorDepth = 4

// findStringDeep returns the first non-empty string value under any of
// `keys`, searching breadth-first from the top of the descriptor.
//
// Why a search rather than a known path: the client descriptor is
// WRAPPED. api packs `stack.Client.Stacks[env]`, so what arrives is
//
//	{"Config": {"config": {"domain": "storage.simple-forge.com", …}},
//	 "template": "lambda", "type": "single-image"}
//
// — the operator's `domain` sits two levels down, under a capitalized
// outer key (an embedded Go field with no json tag) and a lowercase
// inner one. A top-level-only lookup finds nothing, which is exactly
// how the second attempt at this failed: the fix shipped, the plugin
// ran, and `endpoint_url` was still "" (forge-storage deploy
// 44479820610).
//
// Hard-coding "Config.config.domain" would work today and break the
// moment api renames a field or adds a level — in silence, since a
// missing endpoint is indistinguishable from a service that declares
// none. Breadth-first with a depth bound is shape-independent, and
// BREADTH-first specifically: the shallowest match wins, so the
// operator's own `domain` is preferred over any deeper key that happens
// to share the name.
func findStringDeep(root map[string]any, keys ...string) string {
	if root == nil {
		return ""
	}
	type level struct {
		m     map[string]any
		depth int
	}
	queue := []level{{m: root, depth: 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for _, k := range keys {
			if v, ok := cur.m[k].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		if cur.depth >= maxDescriptorDepth {
			continue
		}
		for _, v := range cur.m {
			switch t := v.(type) {
			case map[string]any:
				queue = append(queue, level{m: t, depth: cur.depth + 1})
			case []any:
				for _, item := range t {
					if m, ok := item.(map[string]any); ok {
						queue = append(queue, level{m: m, depth: cur.depth + 1})
					}
				}
			}
		}
	}
	return ""
}

// findMapDeep is findStringDeep for a nested map value — used to locate
// a `healthcheck` block wherever the descriptor keeps it.
func findMapDeep(root map[string]any, key string) map[string]any {
	if root == nil {
		return nil
	}
	type level struct {
		m     map[string]any
		depth int
	}
	queue := []level{{m: root, depth: 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if v, ok := cur.m[key].(map[string]any); ok && len(v) > 0 {
			return v
		}
		if cur.depth >= maxDescriptorDepth {
			continue
		}
		for _, v := range cur.m {
			switch t := v.(type) {
			case map[string]any:
				queue = append(queue, level{m: t, depth: cur.depth + 1})
			case []any:
				for _, item := range t {
					if m, ok := item.(map[string]any); ok {
						queue = append(queue, level{m: m, depth: cur.depth + 1})
					}
				}
			}
		}
	}
	return nil
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
