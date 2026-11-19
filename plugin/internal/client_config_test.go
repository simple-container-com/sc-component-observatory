// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSanitizeClientConfig_StripsSecretsKeepsStructure — the config is
// stored in object storage and served to a browser, so anything
// credential-shaped must not survive, while everything structural
// must (that is the entire reason to keep it).
func TestSanitizeClientConfig_StripsSecretsKeepsStructure(t *testing.T) {
	in := map[string]any{
		"type":     "single-image",
		"template": "lambda",
		"config": map[string]any{
			"domain":    "storage.simple-forge.com",
			"timeout":   30,
			"maxMemory": 512,
			"uses":      []any{"mongodb", "forge-storage", "observatory"},
			// Every one of these must go.
			"signingKey":       "deadbeefdeadbeefdeadbeefdeadbeef",
			"operatorPassword": "hunter2",
			"env": map[string]any{
				"MONGO_URI":    "mongodb+srv://user:pass@host/db",
				"API_TOKEN":    "tok_live_abc",
				"PUBLIC_VALUE": "safe-to-show",
			},
			"secrets": map[string]any{"SOME_SECRET": "${secret:some-alias}"},
		},
	}
	out := SanitizeClientConfig(in)

	// Serialise and scan: a leak anywhere in the tree is what matters,
	// not whether one particular path was handled.
	blob, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(blob)
	for _, leak := range []string{
		"deadbeefdeadbeefdeadbeefdeadbeef",
		"hunter2",
		"tok_live_abc",
		"mongodb+srv://user:pass@host/db",
		"some-alias", // secret ALIASES are a needless disclosure too
	} {
		if strings.Contains(text, leak) {
			t.Errorf("sanitized config still contains %q:\n%s", leak, text)
		}
	}

	// Structure survives.
	cfg := out["config"].(map[string]any)
	if cfg["domain"] != "storage.simple-forge.com" {
		t.Errorf("domain lost: %v", cfg["domain"])
	}
	if cfg["timeout"] != 30 {
		t.Errorf("timeout lost: %v", cfg["timeout"])
	}
	uses, ok := cfg["uses"].([]any)
	if !ok || len(uses) != 3 {
		t.Errorf("uses list lost: %v", cfg["uses"])
	}
	if out["template"] != "lambda" {
		t.Errorf("template lost: %v", out["template"])
	}
	// A non-secret env value is still useful context.
	env := cfg["env"].(map[string]any)
	if env["PUBLIC_VALUE"] != "safe-to-show" {
		t.Errorf("non-secret env value was redacted: %v", env["PUBLIC_VALUE"])
	}

	// A redacted key must still BE there — "unset" and "set but hidden"
	// are different facts for someone debugging a deploy.
	if cfg["signingKey"] != redacted {
		t.Errorf("signingKey should be present-but-redacted, got %v", cfg["signingKey"])
	}
}

// TestSanitizeClientConfig_DoesNotMutateInput — the caller still holds
// the original; mutating it in place would corrupt whatever else reads
// it on the same deploy.
func TestSanitizeClientConfig_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{
		"nested": map[string]any{"password": "secret-value"},
	}
	_ = SanitizeClientConfig(in)
	if got := in["nested"].(map[string]any)["password"]; got != "secret-value" {
		t.Errorf("input was mutated: %v", got)
	}
}

func TestSanitizeClientConfig_NilIsNil(t *testing.T) {
	if SanitizeClientConfig(nil) != nil {
		t.Error("nil in must give nil out, so an absent descriptor stays absent")
	}
}

// TestHealthPathFromClientConfig — a service knows its own probe path
// better than a fleet default does, and it can say so several ways.
func TestHealthPathFromClientConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{
			name: "explicit healthPath",
			cfg:  map[string]any{"healthPath": "/custom/health"},
			want: "/custom/health",
		},
		{
			name: "healthcheck.path",
			cfg:  map[string]any{"healthcheck": map[string]any{"path": "ready"}},
			want: "/ready", // leading slash added
		},
		{
			name: "compose test as string",
			cfg: map[string]any{"healthcheck": map[string]any{
				"test": "curl -f http://localhost:8080/api/health || exit 1",
			}},
			want: "/api/health",
		},
		{
			name: "compose test as vector",
			cfg: map[string]any{"healthcheck": map[string]any{
				"test": []any{"CMD", "wget", "-q", "http://127.0.0.1:3000/healthz"},
			}},
			want: "/healthz",
		},
		{
			name: "nested compose service",
			cfg: map[string]any{"services": map[string]any{
				"web": map[string]any{"healthcheck": map[string]any{"test": "curl http://localhost/up"}},
			}},
			want: "/up",
		},
		{
			name: "URL with no path means root",
			cfg: map[string]any{"healthcheck": map[string]any{
				"test": []any{"CMD-SHELL", "curl -f http://localhost:8080"},
			}},
			want: "/",
		},
		{
			// A non-HTTP probe tells us nothing about a URL. Guessing
			// here would report a healthy service as down.
			name: "non-http probe infers nothing",
			cfg: map[string]any{"healthcheck": map[string]any{
				"test": []any{"CMD", "pg_isready", "-U", "postgres"},
			}},
			want: "",
		},
		{name: "nothing declared", cfg: map[string]any{"template": "lambda"}, want: ""},
		{name: "nil config", cfg: nil, want: ""},
	} {
		if got := HealthPathFromClientConfig(tc.cfg); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestHasURICredential — the narrow guard that caught a real leak: a
// connection string's KEY (MONGO_URI, DATABASE_URL) looks innocuous,
// so key-matching alone let an embedded user:password through.
//
// Deliberately narrow so ordinary values survive: an https URL, a
// host:port pair and a path containing "@" must all pass through
// untouched, or the dashboard loses useful context to over-redaction.
func TestHasURICredential(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"mongodb+srv://user:pass@cluster.mongodb.net/db", true},
		{"postgres://admin:s3cret@10.0.0.1:5432/app", true},
		{"amqp://guest:guest@rabbit:5672/", true},
		// No credentials to hide.
		{"https://storage.simple-forge.com", false},
		{"https://example.com/path/to/thing", false},
		{"mongodb+srv://cluster.mongodb.net/db", false},
		{"redis://cache:6379", false},
		{"host:1234", false},
		{"", false},
		// "@" in the path, not userinfo.
		{"https://example.com/users/@me", false},
	} {
		if got := hasURICredential(tc.in); got != tc.want {
			t.Errorf("hasURICredential(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestEndpointURLFromClientConfig covers the field whose absence left
// the first real tenant unprobeable.
//
// forge-storage registered on 2026-11-19 with a correct record and
// `endpoint_url: ""`, so the health prober had nothing to probe and the
// service sat at `unknown`. The value was in the captured client config
// the whole time (`config.domain: storage.simple-forge.com`) — nothing
// read it.
func TestEndpointURLFromClientConfig(t *testing.T) {
	// The normal case: a bare domain becomes an https base.
	if got := EndpointURLFromClientConfig(map[string]any{
		"domain": "storage.simple-forge.com",
	}); got != "https://storage.simple-forge.com" {
		t.Errorf("domain: got %q", got)
	}

	// An explicit endpoint wins over domain — for a service whose
	// public address is not its `domain`.
	if got := EndpointURLFromClientConfig(map[string]any{
		"domain":      "internal.example.com",
		"endpointUrl": "https://public.example.com/",
	}); got != "https://public.example.com" {
		t.Errorf("explicit endpoint: got %q", got)
	}

	// A compose stack nests the domain per service.
	if got := EndpointURLFromClientConfig(map[string]any{
		"services": map[string]any{
			"web": map[string]any{"domain": "baas.simple-forge.com"},
		},
	}); got != "https://baas.simple-forge.com" {
		t.Errorf("compose service domain: got %q", got)
	}

	// An existing scheme is preserved rather than double-prefixed.
	if got := EndpointURLFromClientConfig(map[string]any{
		"domain": "http://legacy.example.com",
	}); got != "http://legacy.example.com" {
		t.Errorf("explicit scheme: got %q", got)
	}

	// Nothing declared returns "", which the prober records as
	// `unknown` — a wiring gap, not an outage. Returning a guess here
	// would make a never-probed service look DOWN.
	for _, cfg := range []map[string]any{
		nil,
		{},
		{"domain": "   "},
		{"services": map[string]any{"web": map[string]any{"image": "x"}}},
	} {
		if got := EndpointURLFromClientConfig(cfg); got != "" {
			t.Errorf("no endpoint declared (%v): got %q, want empty", cfg, got)
		}
	}
}

// TestEndpointURLFromClientConfig_WrappedDescriptor uses the SHAPE THAT
// ACTUALLY ARRIVES, not a hand-made flat map.
//
// This is the regression that got shipped twice. The first version of
// EndpointURLFromClientConfig read only the top level, which every
// hand-written test fixture satisfied — and the live descriptor does
// not: api packs `stack.Client.Stacks[env]`, so the operator's `domain`
// sits under a capitalized outer key (an embedded Go field with no json
// tag) and a lowercase inner one. The fix shipped green and
// `endpoint_url` was still "" on the live record (forge-storage deploy
// 44479820610).
//
// The fixture below is trimmed from the real captured config served by
// monitor.simple-forge.com for forge-storage--staging.
func TestEndpointURLFromClientConfig_WrappedDescriptor(t *testing.T) {
	live := map[string]any{
		"type":     "single-image",
		"template": "lambda",
		"Config": map[string]any{
			"config": map[string]any{
				"baseDnsZone": "simple-forge.com",
				"domain":      "storage.simple-forge.com",
				"basePath":    "",
				"maxMemory":   float64(512),
				"uses":        []any{"mongodb", "forge-storage", "my-backup", "my-observatory"},
				"cloudExtras": map[string]any{
					"lambdaRoutingType": "function-url",
					"lambdaInvokeMode":  "BUFFERED",
				},
			},
		},
	}

	if got := EndpointURLFromClientConfig(live); got != "https://storage.simple-forge.com" {
		t.Fatalf("wrapped descriptor: got %q, want https://storage.simple-forge.com", got)
	}
}

// TestFindStringDeep_PrefersShallowest pins the breadth-first ordering.
//
// A deep `domain` is usually something else — a key inside an env blob,
// a policy document, a nested service — so the operator's own top-level
// value must win. Depth-first would return whichever branch the map
// iteration happened to walk into, which is non-deterministic in Go and
// would make this flap between deploys.
func TestFindStringDeep_PrefersShallowest(t *testing.T) {
	m := map[string]any{
		"Config": map[string]any{
			"config": map[string]any{
				"domain": "correct.example.com",
				"env": map[string]any{
					"domain": "wrong-deeper.example.com",
				},
			},
		},
	}
	if got := findStringDeep(m, "domain"); got != "correct.example.com" {
		t.Errorf("got %q, want the shallowest match", got)
	}
}

// TestFindStringDeep_DepthBound stops the search wandering.
func TestFindStringDeep_DepthBound(t *testing.T) {
	// 6 levels down — past maxDescriptorDepth, so not found.
	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": map[string]any{"e": map[string]any{"domain": "too-deep"}}}}}}
	if got := findStringDeep(deep, "domain"); got != "" {
		t.Errorf("past the depth bound: got %q, want empty", got)
	}
	// Nil root must not panic.
	if got := findStringDeep(nil, "domain"); got != "" {
		t.Errorf("nil root: got %q", got)
	}
}
