// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
)

// logsWire is re-declared here rather than reusing logsResponse, for the
// same reason deploys_test.go re-declares its shapes: a test that shares
// the handler's struct silently agrees with a field rename instead of
// failing on it, and this is the contract the SPA is written against.
type logsWire struct {
	Stack       string `json:"stack"`
	Provider    string `json:"provider"`
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`
	Events      *[]struct {
		At        time.Time `json:"at"`
		Message   string    `json:"message"`
		Stream    string    `json:"stream"`
		ID        string    `json:"id"`
		Truncated bool      `json:"truncated"`
	} `json:"events"`
	NextCursor  string    `json:"next_cursor"`
	Capped      bool      `json:"capped"`
	CapReason   string    `json:"cap_reason"`
	Scanned     int       `json:"scanned"`
	Calls       int       `json:"calls"`
	Queried     *[]string `json:"queried"`
	Warnings    *[]string `json:"warnings"`
	Unavailable string    `json:"unavailable"`
	Inferred    bool      `json:"inferred"`
	Filter      string    `json:"filter"`
}

type fakeProvider struct {
	name string
	caps telemetry.Capabilities
	page telemetry.LogPage
	err  error

	got    telemetry.LogQuery
	called int
}

func (f *fakeProvider) Name() string                         { return f.name }
func (f *fakeProvider) Capabilities() telemetry.Capabilities { return f.caps }
func (f *fakeProvider) Health(context.Context) error         { return nil }

func (f *fakeProvider) Logs(_ context.Context, q telemetry.LogQuery) (telemetry.LogPage, error) {
	f.got = q
	f.called++
	return f.page, f.err
}

func (f *fakeProvider) Metrics(context.Context, telemetry.MetricQuery) ([]telemetry.Series, error) {
	return nil, telemetry.Unsupported(f.name, "metrics")
}

func (f *fakeProvider) Workloads(context.Context, telemetry.WorkloadQuery) ([]telemetry.Workload, error) {
	return nil, telemetry.Unsupported(f.name, "live workload state")
}

func logsMux(cfg *LogsConfig) *http.ServeMux {
	m := http.NewServeMux()
	auth := JWTMiddleware([]byte(testKey))
	m.Handle("GET /api/v1/services/{stack}/logs", auth(http.HandlerFunc(cfg.Logs)))
	return m
}

// newLogsCfg seeds one registered service and returns the handler config
// plus the fake provider, so a test can assert on the query the handler
// built as well as on the response it rendered.
func newLogsCfg(t *testing.T, cloud string, withSources bool) (*LogsConfig, *fakeProvider) {
	t.Helper()
	store := registry.NewInMemoryStore()
	rec := registry.ServiceRecord{
		Stack: "svc--staging", Service: "svc", Env: "staging",
		Cloud: cloud, Template: "lambda", Version: "v1",
		Inferred: true,
	}
	if withSources {
		rec.Sources = []registry.TelemetrySource{{
			Kind: "cloudwatch-logs", Ref: "/aws/lambda/svc--staging-callback",
			Region: "eu-central-1", Dims: map[string]string{"match": "prefix"},
		}}
	}
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	prov := &fakeProvider{
		name: "aws",
		caps: telemetry.Capabilities{Logs: true},
		page: telemetry.LogPage{
			Events: []telemetry.LogEvent{
				{At: now0, Message: "newest", ID: "2"},
				{At: now0.Add(-time.Minute), Message: "older", ID: "1"},
			},
			Queried: []string{"/aws/lambda/svc--staging-callback-abc"},
			Scanned: 42, Calls: 1,
		},
	}
	cfg := &LogsConfig{
		Registry:  store,
		Providers: telemetry.Set{"aws": prov},
		MaxWindow: 6 * time.Hour,
		MaxLimit:  300,
		Now:       func() time.Time { return now0 },
	}
	return cfg, prov
}

func decodeLogs(t *testing.T, body []byte) logsWire {
	t.Helper()
	var out logsWire
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (body %q)", err, string(body))
	}
	return out
}

// TestLogs_HappyPath — the page, the window the server actually used,
// and the cost of the query all reach the client.
func TestLogs_HappyPath(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true)
	rr := do(t, logsMux(cfg), "GET",
		"/api/v1/services/svc--staging/logs?q=ERROR&minutes=30&limit=50",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeLogs(t, rr.Body.Bytes())

	if got.Stack != "svc--staging" || got.Provider != "aws" {
		t.Errorf("stack/provider = %q/%q", got.Stack, got.Provider)
	}
	if got.Events == nil || len(*got.Events) != 2 {
		t.Fatalf("events = %v, want 2", got.Events)
	}
	if (*got.Events)[0].Message != "newest" {
		t.Error("events must stay in the provider's newest-first order")
	}
	if got.Scanned != 42 || got.Calls != 1 {
		t.Errorf("query cost not surfaced: scanned=%d calls=%d", got.Scanned, got.Calls)
	}
	if got.Filter != "ERROR" {
		t.Errorf("filter not echoed: %q", got.Filter)
	}
	if !got.Inferred {
		t.Error("inferred must be echoed — an empty result from a guessed handle means something different")
	}
	// The window is the server's, echoed, not the client's to recompute.
	wantStart := now0.Add(-30 * time.Minute).Format(time.RFC3339)
	if got.WindowStart != wantStart || got.WindowEnd != now0.Format(time.RFC3339) {
		t.Errorf("window = %s..%s, want %s..%s", got.WindowStart, got.WindowEnd, wantStart, now0.Format(time.RFC3339))
	}
	// And the provider was asked for exactly that window.
	if !prov.got.Start.Equal(now0.Add(-30*time.Minute)) || !prov.got.End.Equal(now0) {
		t.Errorf("provider got %s..%s", prov.got.Start, prov.got.End)
	}
	if prov.got.Limit != 50 || prov.got.Filter != "ERROR" {
		t.Errorf("provider query = limit %d filter %q", prov.got.Limit, prov.got.Filter)
	}
}

// TestLogs_DefaultWindowIsShort — opening a service page must not run a
// wide scan nobody asked for.
func TestLogs_DefaultWindowIsShort(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true)
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if w := prov.got.End.Sub(prov.got.Start); w != logsDefaultWindow {
		t.Errorf("default window = %s, want %s", w, logsDefaultWindow)
	}
}

// TestLogs_RequiresOperator — every tenant's plugin holds a valid
// `register` token, so "valid" is not "authorized": a tenant must not be
// able to read its neighbour's log output (or its own, by this route).
func TestLogs_RequiresOperator(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true)
	m := logsMux(cfg)

	if rr := do(t, m, "GET", "/api/v1/services/svc--staging/logs",
		jwtFor(t, "register", "svc--staging"), nil); rr.Code != http.StatusForbidden {
		t.Errorf("register token: want 403, got %d", rr.Code)
	}
	if rr := do(t, m, "GET", "/api/v1/services/svc--staging/logs",
		jwtFor(t, "scheduler", ""), nil); rr.Code != http.StatusForbidden {
		t.Errorf("scheduler token: want 403, got %d", rr.Code)
	}
	if rr := do(t, m, "GET", "/api/v1/services/svc--staging/logs", "", nil); rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: want 401, got %d", rr.Code)
	}
	if prov.called != 0 {
		t.Error("an unauthorized request reached the provider — a rejected caller must cost no CloudWatch scan")
	}
}

func TestLogs_UnknownStack404(t *testing.T) {
	cfg, _ := newLogsCfg(t, "aws", true)
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/nope--staging/logs",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body["error"] == nil {
		t.Errorf("404 must be a JSON error body, got %q", rr.Body.String())
	}
}

// TestLogs_UnavailableReasonsAre200 — each of these is a reason, not a
// fault. A red error box for "this service has no log handles" teaches
// operators to ignore red error boxes; so they arrive as 200 with an
// `unavailable` string and an EMPTY events array, never as a bare empty
// list and never as a 5xx.
func TestLogs_UnavailableReasonsAre200(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*LogsConfig, *fakeProvider)
		want  string // substring the reason must carry
	}{
		{
			name:  "no handles recorded",
			setup: func(cfg *LogsConfig, _ *fakeProvider) { stripSources(t, cfg) },
			want:  "no telemetry handles",
		},
		{
			name: "cloud not wired",
			setup: func(cfg *LogsConfig, _ *fakeProvider) {
				cfg.Providers = telemetry.Set{}
			},
			want: "aws",
		},
		{
			name: "provider cannot read logs in this build",
			setup: func(_ *LogsConfig, p *fakeProvider) {
				p.caps = telemetry.Capabilities{Logs: false}
			},
			want: "cannot read logs",
		},
		{
			name: "provider reports unavailable",
			setup: func(_ *LogsConfig, p *fakeProvider) {
				p.err = &telemetry.UnavailableError{Reason: "no log group starts with that prefix (inferred)"}
				p.page = telemetry.LogPage{
					Queried:  []string{},
					Warnings: []string{"no log group starts with \"/aws/lambda/x\""},
					Scanned:  7, Calls: 1,
				}
			},
			want: "inferred",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, prov := newLogsCfg(t, "aws", true)
			tc.setup(cfg, prov)
			rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs",
				jwtFor(t, "operator", ""), nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
			}
			got := decodeLogs(t, rr.Body.Bytes())
			if got.Unavailable == "" {
				t.Fatal("200 with no `unavailable` reason is exactly the silent degradation this slice must not ship")
			}
			if !strings.Contains(got.Unavailable, tc.want) {
				t.Errorf("reason = %q, want it to mention %q", got.Unavailable, tc.want)
			}
			if got.Events == nil || len(*got.Events) != 0 {
				t.Errorf("events = %v, want an empty array alongside a reason", got.Events)
			}
		})
	}
}

// TestLogs_ProviderCostSurvivesTheErrorPath — the handles tried and the
// scan spent are the useful part of a failed query, so they must not be
// dropped when the provider also returned an error.
func TestLogs_ProviderCostSurvivesTheErrorPath(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true)
	prov.err = &telemetry.UnavailableError{Reason: "nothing resolved"}
	prov.page = telemetry.LogPage{
		Queried:  []string{"/aws/lambda/a", "/aws/lambda/b"},
		Warnings: []string{"a: AccessDeniedException"},
		Scanned:  13, Calls: 3,
	}
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs",
		jwtFor(t, "operator", ""), nil)
	got := decodeLogs(t, rr.Body.Bytes())
	if got.Queried == nil || len(*got.Queried) != 2 {
		t.Errorf("queried = %v, want both handles", got.Queried)
	}
	if got.Warnings == nil || len(*got.Warnings) != 1 {
		t.Errorf("warnings = %v, want the per-source failure", got.Warnings)
	}
	if got.Scanned != 13 || got.Calls != 3 {
		t.Errorf("cost lost on the error path: scanned=%d calls=%d", got.Scanned, got.Calls)
	}
}

// TestLogs_ProviderFailureIs502 — a genuine read failure is NOT a
// reason. It must not render as "no logs".
func TestLogs_ProviderFailureIs502(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true)
	prov.err = errors.New("every log group failed to read: AccessDeniedException")
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("502 body is not JSON: %q", rr.Body.String())
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "AccessDeniedException") {
		t.Errorf("the AWS text is what the operator acts on; got %q", msg)
	}
}

// TestLogs_BadParamsAre400 — a malformed parameter must be refused, not
// guessed around. A guessed window returns results from a period the UI
// is not labelling.
func TestLogs_BadParamsAre400(t *testing.T) {
	for name, query := range map[string]string{
		"minutes not a number": "?minutes=soon",
		"minutes zero":         "?minutes=0",
		"minutes negative":     "?minutes=-5",
		"limit not a number":   "?limit=all",
		"limit zero":           "?limit=0",
		"start not rfc3339":    "?start=yesterday",
		"end not rfc3339":      "?end=now",
		"inverted window":      "?start=2026-11-18T13:00:00Z&end=2026-11-18T12:00:00Z",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, prov := newLogsCfg(t, "aws", true)
			rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs"+query,
				jwtFor(t, "operator", ""), nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if prov.called != 0 {
				t.Error("a rejected request must not reach the provider")
			}
		})
	}
}

// TestLogs_TooWideWindowIsRefusedNotClamped. Clamping would return
// results from a narrower period than the one the UI is labelling —
// the dashboard misreporting its own axis.
func TestLogs_TooWideWindowIsRefusedNotClamped(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true) // MaxWindow = 6h
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs?minutes=1440",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if prov.called != 0 {
		t.Error("the scan ran despite the window being refused")
	}
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "6h") {
		t.Errorf("the refusal must name the limit so the caller can comply: %q", msg)
	}
}

// TestLogs_LimitIsCappedNotRefused — unlike the window, an oversized
// limit is safe to lower: the page is smaller than asked but covers the
// period the UI labelled, and the cursor carries the rest.
func TestLogs_LimitIsCappedNotRefused(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true) // MaxLimit = 300
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs?limit=99999",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if prov.got.Limit != 300 {
		t.Errorf("provider got limit %d, want it capped to 300", prov.got.Limit)
	}
}

// TestLogs_CursorAndCapReachTheClient — the two fields the "load older"
// control and the honesty banner are built on.
func TestLogs_CursorAndCapReachTheClient(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true)
	prov.page.NextCursor = "eyJiIjoxfQ"
	prov.page.Capped = true
	prov.page.CapReason = "stopped after scanning 20000 events (the scan cap)"
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs?cursor=abc",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeLogs(t, rr.Body.Bytes())
	if got.NextCursor != "eyJiIjoxfQ" {
		t.Errorf("next_cursor = %q", got.NextCursor)
	}
	if !got.Capped || got.CapReason == "" {
		t.Errorf("capped=%v reason=%q — a capped page that looks complete is the failure mode", got.Capped, got.CapReason)
	}
	if prov.got.Cursor != "abc" {
		t.Errorf("cursor not passed through: %q", prov.got.Cursor)
	}
}

// TestLogs_EveryArrayIsSpreadSafe. The SPA has already blanked once on
// `[...undefined]` ("e is not iterable"), with nothing naming the field,
// so this asserts the raw JSON rather than trusting a Go zero value.
func TestLogs_EveryArrayIsSpreadSafe(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true)
	// The worst case: the provider returned bare nils for everything.
	prov.page = telemetry.LogPage{}
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"events", "queried", "warnings"} {
		v, ok := raw[field]
		if !ok {
			t.Errorf("%q is absent from the response — a client spreading it throws", field)
			continue
		}
		if string(v) == "null" {
			t.Errorf("%q serialised as null, which crashes a JS spread", field)
		}
	}
}

// TestLogs_NoRegistry — a route wired before its store is a programming
// error, and 501 says so rather than reporting an empty fleet.
func TestLogs_NoRegistry(t *testing.T) {
	cfg := &LogsConfig{Now: func() time.Time { return now0 }}
	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("want 501, got %d", rr.Code)
	}
}

// stripSources re-seeds the registered service without telemetry
// handles — the "template published none and none could be inferred"
// case.
// stripSources leaves the record with nothing to query.
//
// Clearing Sources is not enough on its own: the handler re-derives
// handles for any record whose template has a known naming convention
// (registry.WithDerivedSources), so a `lambda` record with no sources
// still gets some. The template has to be one nothing can be derived
// from — which is also the only way the real "no handles recorded"
// state is reached now.
func stripSources(t *testing.T, cfg *LogsConfig) {
	t.Helper()
	rec, found, err := cfg.Registry.Get(context.Background(), "svc--staging")
	if err != nil || !found {
		t.Fatalf("reseed: found=%v err=%v", found, err)
	}
	rec.Sources = nil
	rec.Template = "yc-container"
	if err := cfg.Registry.Put(context.Background(), rec); err != nil {
		t.Fatalf("reseed: %v", err)
	}
}

// TestLogs_InferredHandlesAreReDerived — the handler must query this
// build's naming convention, not the guess frozen into the record at
// registration time. That is what made correcting the 2026-11-22 Lambda
// log-group miss a single observatory deploy instead of six tenant
// redeploys.
func TestLogs_InferredHandlesAreReDerived(t *testing.T) {
	cfg, prov := newLogsCfg(t, "aws", true)
	rec, _, err := cfg.Registry.Get(context.Background(), "svc--staging")
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}
	rec.Inferred = true
	rec.Sources = []registry.TelemetrySource{{
		Kind: "cloudwatch-logs",
		Ref:  "/aws/lambda/svc--staging-callback", // the stale wrong guess
		Dims: map[string]string{"match": "prefix"},
	}}
	if err := cfg.Registry.Put(context.Background(), rec); err != nil {
		t.Fatalf("reseed: %v", err)
	}

	rr := do(t, logsMux(cfg), "GET", "/api/v1/services/svc--staging/logs",
		jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var refs []string
	for _, s := range prov.got.Sources {
		refs = append(refs, s.Ref)
	}
	if !slices.Contains(refs, "svc--staging-access-logs") {
		t.Errorf("provider got %v, want the re-derived access-logs group", refs)
	}
	if slices.Contains(refs, "/aws/lambda/svc--staging-callback") {
		t.Errorf("provider got %v, which still carries the stale stored guess", refs)
	}
}
