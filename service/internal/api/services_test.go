// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

const testKey = "a-fake-signing-key-that-is-at-least-32-bytes-long"

var now0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func mux(cfg *ServicesConfig) *http.ServeMux {
	m := http.NewServeMux()
	auth := JWTMiddleware([]byte(testKey))
	m.Handle("GET /api/v1/services", auth(http.HandlerFunc(cfg.List)))
	m.Handle("GET /api/v1/services/{stack}", auth(http.HandlerFunc(cfg.Get)))
	m.Handle("DELETE /api/v1/services/{stack}", auth(http.HandlerFunc(cfg.Delete)))
	m.Handle("POST /api/v1/services/register", auth(http.HandlerFunc(cfg.Register)))
	return m
}

// jwtFor mints a token for the tests.
//
// The timestamps come from real wall-clock, NOT the frozen now0 the
// service clock uses: jwt.ParseWithClaims validates exp/nbf against
// time.Now() and cannot be injected, so a token stamped with a fixed
// past date is simply expired and every request 401s.
func jwtFor(t *testing.T, op, tenant string) string {
	t.Helper()
	real := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"op": op, "tenant": tenant,
		"iat": real.Unix(), "exp": real.Add(time.Hour).Unix(), "nbf": real.Add(-time.Minute).Unix(),
	})
	s, err := tok.SignedString([]byte(testKey))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func do(t *testing.T, m *http.ServeMux, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	m.ServeHTTP(rr, req)
	return rr
}

func newCfg() (*ServicesConfig, *registry.InMemoryStore) {
	store := registry.NewInMemoryStore()
	return &ServicesConfig{Registry: store, Now: func() time.Time { return now0 }}, store
}

func registerBody(stack, version string, withSources bool) map[string]any {
	b := map[string]any{
		"stack": stack, "cloud": "aws", "template": "lambda",
		"version": version, "commit": "4603823b722bea46fc758b89ba775e1857fc94d9",
		"endpoint_url": "https://" + stack + ".example.com",
	}
	if withSources {
		b["sources"] = []map[string]any{{
			"kind": "cloudwatch-logs", "ref": "/aws/lambda/" + stack,
			"region": "eu-central-1", "dims": map[string]string{"FunctionName": stack},
		}}
	}
	return b
}

// TestRegister_HappyPath — a push-register lands a complete row.
func TestRegister_HappyPath(t *testing.T) {
	cfg, store := newCfg()
	rr := do(t, mux(cfg), "POST", "/api/v1/services/register",
		jwtFor(t, "register", "forge-notifier--staging"),
		registerBody("forge-notifier--staging", "2026.11.18-abc1234", true))
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rec, found, _ := store.Get(context.Background(), "forge-notifier--staging")
	if !found {
		t.Fatal("not persisted")
	}
	if rec.Service != "forge-notifier" || rec.Env != "staging" {
		t.Errorf("split wrong: %q / %q", rec.Service, rec.Env)
	}
	if rec.Version != "2026.11.18-abc1234" || len(rec.Sources) != 1 {
		t.Errorf("record wrong: %+v", rec)
	}
}

// TestRegister_RejectsCrossTenantToken — a consumer must not be able to
// register under another consumer's stack name with its own token.
func TestRegister_RejectsCrossTenantToken(t *testing.T) {
	cfg, _ := newCfg()
	rr := do(t, mux(cfg), "POST", "/api/v1/services/register",
		jwtFor(t, "register", "attacker--staging"),
		registerBody("victim--staging", "v1", true))
	if rr.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestRegister_RejectsWrongOp — an operator session token must not be
// usable to register a service.
func TestRegister_RejectsWrongOp(t *testing.T) {
	cfg, _ := newCfg()
	rr := do(t, mux(cfg), "POST", "/api/v1/services/register",
		jwtFor(t, "operator", "svc--staging"),
		registerBody("svc--staging", "v1", true))
	if rr.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", rr.Code)
	}
}

// TestRegister_NoCredential_401
func TestRegister_NoCredential_401(t *testing.T) {
	cfg, _ := newCfg()
	rr := do(t, mux(cfg), "POST", "/api/v1/services/register", "",
		registerBody("svc--staging", "v1", true))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", rr.Code)
	}
}

// TestRegister_EmptySourcesDoesNotWipe — the API-level guard for the
// sc-component-backup incident: a re-register that observed no
// telemetry must not erase known-good handles.
func TestRegister_EmptySourcesDoesNotWipe(t *testing.T) {
	cfg, store := newCfg()
	m := mux(cfg)
	tok := jwtFor(t, "register", "svc--staging")

	if rr := do(t, m, "POST", "/api/v1/services/register", tok,
		registerBody("svc--staging", "v1", true)); rr.Code != http.StatusOK {
		t.Fatalf("seed: %d", rr.Code)
	}
	if rr := do(t, m, "POST", "/api/v1/services/register", tok,
		registerBody("svc--staging", "v2", false)); rr.Code != http.StatusOK {
		t.Fatalf("second: %d", rr.Code)
	}

	rec, _, _ := store.Get(context.Background(), "svc--staging")
	if len(rec.Sources) != 1 {
		t.Errorf("sources wiped by a discovery miss: %d", len(rec.Sources))
	}
	if rec.Version != "v2" {
		t.Errorf("version should still advance: %q", rec.Version)
	}
}

// TestList_ShapeIsSpreadSafe — every array the UI touches must be
// present and non-null. The first SC Component crashed its SPA on one
// absent array field (`[...undefined]` → "e is not iterable"), so this
// asserts the JSON directly rather than trusting the Go zero value.
func TestList_ShapeIsSpreadSafe(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	// A service with NO sources — the dangerous case.
	if rr := do(t, m, "POST", "/api/v1/services/register",
		jwtFor(t, "register", "bare--staging"),
		registerBody("bare--staging", "v1", false)); rr.Code != http.StatusOK {
		t.Fatalf("seed: %d", rr.Code)
	}

	rr := do(t, m, "GET", "/api/v1/services", jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var resp struct {
		Count    int `json:"count"`
		Services []struct {
			Sources        *[]sourceJSON `json:"sources"`
			TelemetryReady bool          `json:"telemetry_ready"`
			ShortCommit    string        `json:"short_commit"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("want 1 service, got %d", resp.Count)
	}
	s := resp.Services[0]
	if s.Sources == nil {
		t.Error("sources must serialise as [] not null — a null crashes a JS spread")
	}
	if s.TelemetryReady {
		t.Error("a service with no sources must report telemetry_ready=false")
	}
	if s.ShortCommit != "4603823" {
		t.Errorf("short_commit computed server-side: got %q", s.ShortCommit)
	}
}

// TestGet_IncludesDeployTimeline
func TestGet_IncludesDeployTimeline(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	tok := jwtFor(t, "register", "svc--staging")
	for _, v := range []string{"v1", "v2"} {
		if rr := do(t, m, "POST", "/api/v1/services/register", tok,
			registerBody("svc--staging", v, true)); rr.Code != http.StatusOK {
			t.Fatalf("seed %s: %d", v, rr.Code)
		}
	}

	rr := do(t, m, "GET", "/api/v1/services/svc--staging", jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Service serviceJSON            `json:"service"`
		Deploys []registry.DeployEvent `json:"deploys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Service.Version != "v2" {
		t.Errorf("current version: %q", resp.Service.Version)
	}
	if len(resp.Deploys) != 2 {
		t.Fatalf("want 2 deploy events, got %d", len(resp.Deploys))
	}
	if resp.Deploys[0].Version != "v2" || resp.Deploys[0].PreviousVersion != "v1" {
		t.Errorf("timeline newest-first with transition: %+v", resp.Deploys[0])
	}
}

func TestGet_Unknown_404(t *testing.T) {
	cfg, _ := newCfg()
	rr := do(t, mux(cfg), "GET", "/api/v1/services/nope--staging", jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestDelete_HappyPathAndTypo — removal works, and a typo is a visible
// 404 rather than a silent success.
func TestDelete_HappyPathAndTypo(t *testing.T) {
	cfg, store := newCfg()
	m := mux(cfg)
	if rr := do(t, m, "POST", "/api/v1/services/register",
		jwtFor(t, "register", "gone--staging"),
		registerBody("gone--staging", "v1", true)); rr.Code != http.StatusOK {
		t.Fatalf("seed: %d", rr.Code)
	}

	if rr := do(t, m, "DELETE", "/api/v1/services/gone--staging",
		jwtFor(t, "operator", ""), nil); rr.Code != http.StatusOK {
		t.Fatalf("delete: %d", rr.Code)
	}
	if _, found, _ := store.Get(context.Background(), "gone--staging"); found {
		t.Error("record survived delete")
	}
	if rr := do(t, m, "DELETE", "/api/v1/services/typo--staging",
		jwtFor(t, "operator", ""), nil); rr.Code != http.StatusNotFound {
		t.Errorf("typo should 404, got %d", rr.Code)
	}
}

// TestErrorsAreJSON — every error path must be parseable JSON. The
// first SC Component served HTML 404s to a JSON-expecting SPA and the
// resulting crash was unreadable.
func TestErrorsAreJSON(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	for _, tc := range []struct{ method, path, token string }{
		{"GET", "/api/v1/services", ""},                                      // 401
		{"GET", "/api/v1/services/nope--staging", jwtFor(t, "operator", "")}, // 404
	} {
		rr := do(t, m, tc.method, tc.path, tc.token, nil)
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s (status %d) body is not JSON: %q", tc.method, tc.path, rr.Code, rr.Body.String())
			continue
		}
		if _, ok := body["error"]; !ok {
			t.Errorf("%s %s: error body missing 'error' key: %v", tc.method, tc.path, body)
		}
	}
}
