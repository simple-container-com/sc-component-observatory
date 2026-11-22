// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
)

// metricsWire is re-declared rather than reusing metricsResponse, for
// the same reason logsWire is: a test that shares the handler's struct
// silently agrees with a field rename instead of failing on it, and this
// is the contract the SPA is written against.
//
// Every slice is a POINTER to a slice so a missing JSON field is
// distinguishable from an empty array. `undefined` reaching a `.map()`
// in the SPA blanks the page with nothing naming the field, which this
// component has paid for once already.
type metricsWire struct {
	Stack         string `json:"stack"`
	Provider      string `json:"provider"`
	WindowStart   string `json:"window_start"`
	WindowEnd     string `json:"window_end"`
	PeriodSeconds int    `json:"period_seconds"`
	Stat          string `json:"stat"`
	Adjusted      bool   `json:"adjusted"`
	AdjustReason  string `json:"adjust_reason"`
	Series        *[]struct {
		Name   string            `json:"name"`
		Unit   string            `json:"unit"`
		Stat   string            `json:"stat"`
		Label  string            `json:"label"`
		Dims   map[string]string `json:"dims"`
		Absent bool              `json:"absent"`
		Reason string            `json:"reason"`
		Points *[]struct {
			At    time.Time `json:"at"`
			Value float64   `json:"value"`
		} `json:"points"`
	} `json:"series"`
	Capped      bool      `json:"capped"`
	CapReason   string    `json:"cap_reason"`
	Calls       int       `json:"calls"`
	Queried     *[]string `json:"queried"`
	Warnings    *[]string `json:"warnings"`
	Unavailable string    `json:"unavailable"`
	Inferred    bool      `json:"inferred"`
}

// fakeMetricProvider records the query the handler built and returns a
// canned result, so the parsing and the honesty fields can be asserted
// without an AWS fake.
type fakeMetricProvider struct {
	name string
	caps telemetry.Capabilities
	res  telemetry.MetricResult
	err  error

	got    telemetry.MetricQuery
	called int
}

func (f *fakeMetricProvider) Name() string                         { return f.name }
func (f *fakeMetricProvider) Capabilities() telemetry.Capabilities { return f.caps }
func (f *fakeMetricProvider) Health(context.Context) error         { return nil }

func (f *fakeMetricProvider) Logs(context.Context, telemetry.LogQuery) (telemetry.LogPage, error) {
	return telemetry.LogPage{}, telemetry.Unsupported(f.name, "logs")
}

func (f *fakeMetricProvider) Metrics(_ context.Context, q telemetry.MetricQuery) (telemetry.MetricResult, error) {
	f.got = q
	f.called++
	return f.res, f.err
}

func (f *fakeMetricProvider) Workloads(context.Context, telemetry.WorkloadQuery) ([]telemetry.Workload, error) {
	return nil, telemetry.Unsupported(f.name, "live workload state")
}

func metricsMux(cfg *MetricsConfig) *http.ServeMux {
	m := http.NewServeMux()
	auth := JWTMiddleware([]byte(testKey))
	m.Handle("GET /api/v1/services/{stack}/metrics", auth(http.HandlerFunc(cfg.Metrics)))
	return m
}

func newMetricsCfg(t *testing.T, cloud string, withSources bool) (*MetricsConfig, *fakeMetricProvider) {
	t.Helper()
	store := registry.NewInMemoryStore()
	rec := registry.ServiceRecord{
		Stack: "svc--staging", Service: "svc", Env: "staging",
		Cloud: cloud, Version: "v1",
		Inferred: true,
	}
	if withSources {
		// The template matters: the handler re-derives inferred handles
		// (registry.WithDerivedSources), so a record with a template SC
		// has conventions for can never reach the "no handles" branch —
		// which is why the no-sources case below leaves it empty.
		rec.Template = "lambda"
		rec.Sources = []registry.TelemetrySource{{
			Kind: registry.KindCloudWatchMetrics, Ref: "AWS/Lambda", Region: "eu-central-1",
			Dims: map[string]string{"match": "prefix", "FunctionNamePrefix": "svc--staging-callback"},
		}}
	}
	if err := store.Put(context.Background(), rec); err != nil {
		t.Fatalf("seed: %v", err)
	}
	prov := &fakeMetricProvider{
		name: "aws",
		caps: telemetry.Capabilities{Logs: true, Metrics: true},
		res: telemetry.MetricResult{
			Series: []telemetry.Series{
				{
					Name: "Invocations", Unit: "Count", Stat: "Sum", Label: "Invocations",
					Dims: map[string]string{"FunctionName": "svc--staging-callback-ab1"},
					Points: []telemetry.Point{
						{At: now0.Add(-2 * time.Minute), Value: 3},
						{At: now0.Add(-time.Minute), Value: 0},
					},
				},
				{
					Name: "Errors", Stat: "Sum", Label: "Errors",
					Points: []telemetry.Point{},
					Absent: true,
					Reason: "CloudWatch lists no Errors metric for svc--staging-callback-ab1",
				},
			},
			PeriodSeconds: 60,
			Queried:       []string{"svc--staging-callback-ab1"},
			Warnings:      []string{},
			Calls:         2,
		},
	}
	cfg := &MetricsConfig{
		Registry:  store,
		Providers: telemetry.Set{"aws": prov},
		MaxWindow: 7 * 24 * time.Hour,
		Now:       func() time.Time { return now0 },
	}
	return cfg, prov
}

func decodeMetrics(t *testing.T, body []byte) metricsWire {
	t.Helper()
	var out metricsWire
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (body %q)", err, string(body))
	}
	return out
}

// TestMetrics_HappyPath — the series, the window actually used, the
// period actually used, and the cost all reach the client.
func TestMetrics_HappyPath(t *testing.T) {
	cfg, prov := newMetricsCfg(t, registry.CloudAWS, true)
	rr := do(t, metricsMux(cfg), "GET", "/api/v1/services/svc--staging/metrics", jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	out := decodeMetrics(t, rr.Body.Bytes())
	if out.Series == nil || len(*out.Series) != 2 {
		t.Fatalf("series = %+v, want both the present and the absent one", out.Series)
	}
	if out.PeriodSeconds != 60 {
		t.Errorf("period_seconds = %d, want the provider's value echoed", out.PeriodSeconds)
	}
	if out.Calls != 2 {
		t.Errorf("calls = %d, want the query cost surfaced", out.Calls)
	}
	if out.Queried == nil || len(*out.Queried) != 1 {
		t.Errorf("queried = %+v, want the charted target named", out.Queried)
	}
	if !out.Inferred {
		t.Error("inferred must be echoed: a derived handle that matched nothing is a wrong guess, not an idle service")
	}
	// The default window must be the handler's, not the client's idea
	// of one.
	if prov.got.Start.IsZero() || !prov.got.Start.Before(prov.got.End) {
		t.Errorf("query window = %s..%s", prov.got.Start, prov.got.End)
	}
	if out.WindowEnd != now0.Format(time.RFC3339) {
		t.Errorf("window_end = %q, want the frozen now", out.WindowEnd)
	}
	// Period unset ⇒ the provider chooses; the handler must not invent
	// one, because the provider is what knows the retention ladder.
	if prov.got.Period != 0 {
		t.Errorf("Period = %s, want zero so the provider chooses", prov.got.Period)
	}
}

// TestMetrics_AbsentSeriesSurvives — the central contract. An absent
// metric must arrive flagged, with its reason and a real (empty) points
// array.
func TestMetrics_AbsentSeriesSurvives(t *testing.T) {
	cfg, _ := newMetricsCfg(t, registry.CloudAWS, true)
	rr := do(t, metricsMux(cfg), "GET", "/api/v1/services/svc--staging/metrics", jwtFor(t, "operator", ""), nil)
	out := decodeMetrics(t, rr.Body.Bytes())
	s := (*out.Series)[1]
	if !s.Absent || s.Reason == "" {
		t.Fatalf("absent series lost its flag or reason: %+v", s)
	}
	if s.Points == nil {
		t.Error("points must be [] and never null — the SPA maps over it")
	}
}

// TestMetrics_NeverNullArrays — every slice on the wire is a real array
// even on the refusal paths.
func TestMetrics_NeverNullArrays(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cloud string
		srcs  bool
		caps  telemetry.Capabilities
		err   error
	}{
		{"no provider for the cloud", "yandex", true, telemetry.Capabilities{Metrics: true}, nil},
		{"provider cannot do metrics", registry.CloudAWS, true, telemetry.Capabilities{Logs: true}, nil},
		{"no handles recorded", registry.CloudAWS, false, telemetry.Capabilities{Metrics: true}, nil},
		{"provider refuses", registry.CloudAWS, true, telemetry.Capabilities{Metrics: true}, &telemetry.UnavailableError{Reason: "never invoked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, prov := newMetricsCfg(t, tc.cloud, tc.srcs)
			prov.caps = tc.caps
			prov.err = tc.err
			if tc.err != nil {
				prov.res = telemetry.MetricResult{}
			}
			rr := do(t, metricsMux(cfg), "GET", "/api/v1/services/svc--staging/metrics", jwtFor(t, "operator", ""), nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: an unavailable read is a reason, not a fault", rr.Code)
			}
			out := decodeMetrics(t, rr.Body.Bytes())
			if out.Series == nil || out.Queried == nil || out.Warnings == nil {
				t.Fatalf("null array on the wire: %s", rr.Body.String())
			}
			if out.Unavailable == "" {
				t.Error("an empty result must carry a reason")
			}
		})
	}
}

// TestMetrics_AdjustedPeriodIsReported — a raised period must reach the
// UI. The failure it prevents is a chart drawn at a resolution the data
// never had.
func TestMetrics_AdjustedPeriodIsReported(t *testing.T) {
	cfg, prov := newMetricsCfg(t, registry.CloudAWS, true)
	prov.res.Adjusted = true
	prov.res.AdjustReason = "the period was raised from 1m to 5m"
	prov.res.PeriodSeconds = 300
	rr := do(t, metricsMux(cfg), "GET", "/api/v1/services/svc--staging/metrics?period=60&minutes=60", jwtFor(t, "operator", ""), nil)
	out := decodeMetrics(t, rr.Body.Bytes())
	if !out.Adjusted || out.AdjustReason == "" {
		t.Fatal("the adjustment must be on the response, not only in a log line")
	}
	if out.PeriodSeconds != 300 {
		t.Errorf("period_seconds = %d, want the period that RAN", out.PeriodSeconds)
	}
	if prov.got.Period != time.Minute {
		t.Errorf("requested period = %s, want the caller's 60s passed through", prov.got.Period)
	}
}

// TestMetrics_ParamsReachTheProvider — names, stat and the explicit
// window are forwarded rather than reinterpreted.
func TestMetrics_ParamsReachTheProvider(t *testing.T) {
	cfg, prov := newMetricsCfg(t, registry.CloudAWS, true)
	path := "/api/v1/services/svc--staging/metrics?names=Invocations,%20Errors&stat=Maximum&minutes=30"
	rr := do(t, metricsMux(cfg), "GET", path, jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if len(prov.got.Names) != 2 || prov.got.Names[0] != "Invocations" || prov.got.Names[1] != "Errors" {
		t.Errorf("names = %v, want them split and trimmed", prov.got.Names)
	}
	if prov.got.Stat != "Maximum" {
		t.Errorf("stat = %q", prov.got.Stat)
	}
	if got := prov.got.End.Sub(prov.got.Start); got != 30*time.Minute {
		t.Errorf("window = %s, want 30m", got)
	}
}

// TestMetrics_RejectsBadParams — refused, not silently defaulted: a
// request answered with a different window than it asked for is the
// dashboard misreporting its own axis.
func TestMetrics_RejectsBadParams(t *testing.T) {
	cfg, _ := newMetricsCfg(t, registry.CloudAWS, true)
	m := metricsMux(cfg)
	for _, q := range []string{
		"?period=0", "?period=abc", "?period=-5",
		"?minutes=0", "?minutes=notanumber",
		"?start=yesterday", "?end=soon",
		"?minutes=20160", // wider than MaxWindow
	} {
		rr := do(t, m, "GET", "/api/v1/services/svc--staging/metrics"+q, jwtFor(t, "operator", ""), nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rr.Code)
		}
	}
}

// TestMetrics_RequiresOperatorToken — a tenant's own register token must
// not read another tenant's traffic numbers.
func TestMetrics_RequiresOperatorToken(t *testing.T) {
	cfg, _ := newMetricsCfg(t, registry.CloudAWS, true)
	m := metricsMux(cfg)
	rr := do(t, m, "GET", "/api/v1/services/svc--staging/metrics", jwtFor(t, "register", "svc--staging"), nil)
	if rr.Code != http.StatusForbidden {
		t.Errorf("tenant token: status = %d, want 403", rr.Code)
	}
	rr = do(t, m, "GET", "/api/v1/services/svc--staging/metrics", "", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", rr.Code)
	}
}

// TestMetrics_UnknownService404s — and does NOT fall through to the SPA
// shell, which would answer 200 with HTML.
func TestMetrics_UnknownService404s(t *testing.T) {
	cfg, _ := newMetricsCfg(t, registry.CloudAWS, true)
	rr := do(t, metricsMux(cfg), "GET", "/api/v1/services/nope--staging/metrics", jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

// TestMetrics_HardFailureIsAnError — a genuine fault must not render as
// an empty chart with a caption.
func TestMetrics_HardFailureIsAnError(t *testing.T) {
	cfg, prov := newMetricsCfg(t, registry.CloudAWS, true)
	prov.err = errors.New("AccessDeniedException: cloudwatch:GetMetricData")
	rr := do(t, metricsMux(cfg), "GET", "/api/v1/services/svc--staging/metrics", jwtFor(t, "operator", ""), nil)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
}
