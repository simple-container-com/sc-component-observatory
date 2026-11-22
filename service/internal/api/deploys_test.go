// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/simple-container-com/sc-component-observatory/service/internal/configdiff"
)

// deploysResponse mirrors the wire shape. Declared here rather than
// reusing the handler's own types so a field rename shows up as a test
// failure instead of silently agreeing with itself.
type deploysResponse struct {
	Stack   string `json:"stack"`
	Count   int    `json:"count"`
	Deploys []struct {
		ID             string           `json:"id"`
		Version        string           `json:"version"`
		ShortCommit    string           `json:"short_commit"`
		ConfigCaptured bool             `json:"config_captured"`
		Diff           *configdiff.Diff `json:"diff"`
	} `json:"deploys"`
}

// registerBodyWithConfig is registerBody plus the client-config
// snapshot the plugin sends — `maxMemory` under the real two-level
// wrapper (`Config.config`), because that is the shape the api module
// actually packs.
func registerBodyWithConfig(stack, version string, maxMemory int) map[string]any {
	b := registerBody(stack, version, true)
	b["client_config"] = map[string]any{
		"template": "lambda",
		"Config": map[string]any{
			"config": map[string]any{
				"maxMemory": maxMemory,
				"env":       map[string]any{"LOG_LEVEL": "info"},
			},
		},
	}
	return b
}

func seedDeploys(t *testing.T, m *http.ServeMux, stack string, versions ...map[string]any) {
	t.Helper()
	tok := jwtFor(t, "register", stack)
	for i, body := range versions {
		rr := do(t, m, "POST", "/api/v1/services/register", tok, body)
		if rr.Code != http.StatusOK {
			t.Fatalf("seed %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
}

func getDeploys(t *testing.T, m *http.ServeMux, path string) (*deploysResponse, int) {
	t.Helper()
	rr := do(t, m, "GET", path, jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		return nil, rr.Code
	}
	var resp deploysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, rr.Body.String())
	}
	return &resp, rr.Code
}

// TestDeploys_DiffsAgainstThePreviousDeploy — the slice in one test:
// two deploys with different configs produce a diff on the newer row,
// bucketed, with the previous and current values both present.
func TestDeploys_DiffsAgainstThePreviousDeploy(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	seedDeploys(
		t, m, "svc--staging",
		registerBodyWithConfig("svc--staging", "v1", 512),
		registerBodyWithConfig("svc--staging", "v2", 1024),
	)

	resp, code := getDeploys(t, m, "/api/v1/services/svc--staging/deploys")
	if resp == nil {
		t.Fatalf("want 200, got %d", code)
	}
	if resp.Stack != "svc--staging" || resp.Count != 2 || len(resp.Deploys) != 2 {
		t.Fatalf("want 2 rows for svc--staging: %+v", resp)
	}

	newest := resp.Deploys[0]
	if newest.Version != "v2" {
		t.Fatalf("not newest-first: %q", newest.Version)
	}
	if !newest.ConfigCaptured {
		t.Error("config_captured should be true for a deploy that sent a config")
	}
	if newest.Diff == nil {
		t.Fatal("diff missing")
	}
	if newest.Diff.Unavailable != "" {
		t.Fatalf("diff should be computable: %s", newest.Diff.Unavailable)
	}
	if len(newest.Diff.Compute) != 1 {
		t.Fatalf("want 1 compute change, got %+v", newest.Diff)
	}
	ch := newest.Diff.Compute[0]
	if ch.Key != "maxMemory" || ch.Previous != "512" || ch.Current != "1024" {
		t.Errorf("wrong change: %+v", ch)
	}
	if newest.ShortCommit != "4603823" {
		t.Errorf("short_commit: %q", newest.ShortCommit)
	}
}

// TestDeploys_FirstDeployStatesWhyItHasNoDiff — the oldest row must
// explain itself. An empty diff there would read as "this deploy
// changed nothing", which is the silent-degradation failure the
// component exists to stop.
func TestDeploys_FirstDeployStatesWhyItHasNoDiff(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	seedDeploys(t, m, "svc--staging", registerBodyWithConfig("svc--staging", "v1", 512))

	resp, code := getDeploys(t, m, "/api/v1/services/svc--staging/deploys")
	if resp == nil {
		t.Fatalf("want 200, got %d", code)
	}
	if len(resp.Deploys) != 1 {
		t.Fatalf("want 1 row, got %d", len(resp.Deploys))
	}
	d := resp.Deploys[0].Diff
	if d == nil {
		t.Fatal("diff object must be present even when it cannot be computed")
	}
	if d.Unavailable == "" {
		t.Error("the first deploy must say WHY there is no diff")
	}
	if d.Env == nil || d.Compute == nil || d.Settings == nil || d.Artifact == nil {
		t.Error("buckets must serialise as [] not null — a null crashes a JS .map()")
	}
}

// TestDeploys_MissingSnapshotIsExplained — every deploy that predates
// this slice has no snapshot, which is the common case on the live
// dashboard right after shipping it.
func TestDeploys_MissingSnapshotIsExplained(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	seedDeploys(
		t, m, "svc--staging",
		registerBody("svc--staging", "v1", true), // no client_config: pre-slice deploy
		registerBodyWithConfig("svc--staging", "v2", 1024),
	)

	resp, _ := getDeploys(t, m, "/api/v1/services/svc--staging/deploys")
	if resp == nil || len(resp.Deploys) != 2 {
		t.Fatalf("want 2 rows: %+v", resp)
	}
	newest := resp.Deploys[0]
	if newest.Version != "v2" || !newest.ConfigCaptured {
		t.Fatalf("precondition: %+v", newest)
	}
	if newest.Diff.Unavailable == "" {
		t.Fatalf("a deploy whose predecessor has no snapshot must say so, got %d changes",
			newest.Diff.Count())
	}
	if newest.Diff.Count() != 0 {
		t.Error("an unavailable diff must not also carry changes")
	}
	if resp.Deploys[1].ConfigCaptured {
		t.Error("the pre-slice deploy must not claim a snapshot")
	}
}

// TestDeploys_PageBoundaryUsesAnOffPageBaseline — the oldest row of a
// page has a previous deploy; it is just not on the page. Reporting
// "nothing to compare against" there would be true of the page and
// false of the service, so the handler reads one event beyond it.
func TestDeploys_PageBoundaryUsesAnOffPageBaseline(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	bodies := make([]map[string]any, 0, 3)
	for i, mem := range []int{256, 512, 1024} {
		bodies = append(bodies, registerBodyWithConfig("svc--staging", fmt.Sprintf("v%d", i+1), mem))
	}
	seedDeploys(t, m, "svc--staging", bodies...)

	resp, _ := getDeploys(t, m, "/api/v1/services/svc--staging/deploys?limit=2")
	if resp == nil {
		t.Fatal("no response")
	}
	if resp.Count != 2 {
		t.Fatalf("limit not honoured: %d rows", resp.Count)
	}
	oldestOnPage := resp.Deploys[1]
	if oldestOnPage.Version != "v2" {
		t.Fatalf("unexpected page: %+v", resp.Deploys)
	}
	if oldestOnPage.Diff.Unavailable != "" {
		t.Errorf("the off-page baseline was not used: %s", oldestOnPage.Diff.Unavailable)
	}
	if len(oldestOnPage.Diff.Compute) != 1 || oldestOnPage.Diff.Compute[0].Previous != "256" {
		t.Errorf("diffed against the wrong baseline: %+v", oldestOnPage.Diff)
	}
}

// TestDeploys_SecretRotationShowsWithoutLeaking — end to end through
// the registration path: the plugin's redaction fingerprints survive
// the snapshot round-trip and surface as a flagged change, with no
// fingerprint in the response body.
func TestDeploys_SecretRotationShowsWithoutLeaking(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)

	withSecret := func(version, fingerprint string) map[string]any {
		b := registerBody("svc--staging", version, true)
		b["client_config"] = map[string]any{
			"Config": map[string]any{"config": map[string]any{
				"env": map[string]any{"API_KEY": "[redacted:" + fingerprint + "]"},
			}},
		}
		return b
	}
	seedDeploys(t, m, "svc--staging", withSecret("v1", "aaaaaa"), withSecret("v2", "bbbbbb"))

	rr := do(t, m, "GET", "/api/v1/services/svc--staging/deploys", jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	for _, fp := range []string{"aaaaaa", "bbbbbb"} {
		if strings.Contains(body, fp) {
			t.Errorf("fingerprint %q leaked into the response", fp)
		}
	}

	var resp deploysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	d := resp.Deploys[0].Diff
	if len(d.Env) != 1 || !d.Env[0].Secret {
		t.Fatalf("rotation not reported as a secret change: %+v", d)
	}
	if d.Env[0].Key != "API_KEY" {
		t.Errorf("key: %q", d.Env[0].Key)
	}
}

// TestDeploys_NoEventForAnUnchangedVersion — a re-register of the same
// version is not a deploy, so it must not produce a phantom row (and so
// cannot produce a phantom "nothing changed" diff either).
func TestDeploys_NoEventForAnUnchangedVersion(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	seedDeploys(
		t, m, "svc--staging",
		registerBodyWithConfig("svc--staging", "v1", 512),
		registerBodyWithConfig("svc--staging", "v1", 512),
	)
	resp, _ := getDeploys(t, m, "/api/v1/services/svc--staging/deploys")
	if resp == nil || resp.Count != 1 {
		t.Errorf("want 1 row, got %+v", resp)
	}
}

func TestDeploys_UnknownStackIs404(t *testing.T) {
	cfg, _ := newCfg()
	rr := do(t, mux(cfg), "GET", "/api/v1/services/nope--staging/deploys",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rr.Code)
	}
}

// TestDeploys_LimitValidation — a bad limit is a visible 400, not a
// silent fallback to the default, and a huge one is clamped rather than
// turned into 10k snapshot GETs.
func TestDeploys_LimitValidation(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	seedDeploys(t, m, "svc--staging", registerBodyWithConfig("svc--staging", "v1", 512))

	for _, bad := range []string{"0", "-1", "abc", "1.5"} {
		rr := do(t, m, "GET", "/api/v1/services/svc--staging/deploys?limit="+bad,
			jwtFor(t, "operator", ""), nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("limit=%q: want 400, got %d", bad, rr.Code)
		}
	}
	// Over the cap is clamped, not rejected: the caller asked for "all",
	// and refusing that is less useful than serving the maximum page.
	if _, code := getDeploys(t, m, "/api/v1/services/svc--staging/deploys?limit=100000"); code != http.StatusOK {
		t.Errorf("an over-cap limit should be clamped, got %d", code)
	}
}

// TestDeploys_RequiresOperatorToken — every consumer's plugin holds a
// valid `register` token for this service. If that token could read
// this route, one tenant could read every other tenant's config diffs,
// which is exactly what the token's `tenant` scoping exists to prevent.
func TestDeploys_RequiresOperatorToken(t *testing.T) {
	cfg, _ := newCfg()
	m := mux(cfg)
	seedDeploys(t, m, "svc--staging", registerBodyWithConfig("svc--staging", "v1", 512))

	for _, path := range []string{
		"/api/v1/services",
		"/api/v1/services/svc--staging",
		"/api/v1/services/svc--staging/deploys",
	} {
		// A valid token for ANOTHER tenant's registration.
		rr := do(t, m, "GET", path, jwtFor(t, "register", "attacker--staging"), nil)
		if rr.Code != http.StatusForbidden {
			t.Errorf("GET %s with a register token: want 403, got %d", path, rr.Code)
		}
		if rr := do(t, m, "GET", path, "", nil); rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no token: want 401, got %d", path, rr.Code)
		}
	}
}
