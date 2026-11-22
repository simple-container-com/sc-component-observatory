// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
)

// MetricsConfig wires the metric-query handler.
type MetricsConfig struct {
	Registry  registry.Store
	Providers telemetry.Set

	// DefaultWindow is the lookback applied when the caller names none.
	// Wider than the log default: a metric read costs a fixed number of
	// API calls regardless of the window, so the reason to keep the log
	// default short (scan volume) does not apply here, and three hours
	// is the narrowest window in which a 1-minute series shows a shape.
	DefaultWindow time.Duration

	// MaxWindow is the widest window accepted. REFUSED beyond it rather
	// than clamped, for the same reason logs are: a clamped window
	// returns a different period than the one the UI labels.
	//
	// Note this is NOT the retention limit — the provider raises the
	// period for an old window and says so. This bound is about the
	// response size and the honesty of the axis.
	MaxWindow time.Duration

	// MaxSeries caps the fan-out. Zero means the provider's default.
	MaxSeries int

	// Now is injected for tests.
	Now func() time.Time
}

// Fallbacks, used when the operator set no env override.
const (
	metricsDefaultWindow = 3 * time.Hour
	metricsMaxWindow     = 30 * 24 * time.Hour
)

func (c *MetricsConfig) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

// metricsResponse is the wire shape.
//
// Built around the same rule as logsResponse: a reader must never be
// unable to tell "nothing was emitted" from "we could not look". So the
// window, the period that actually ran, every target that was charted,
// every per-source failure and every absent metric are all on the
// response — and `series` is never null.
type metricsResponse struct {
	Stack    string `json:"stack"`
	Provider string `json:"provider,omitempty"`

	// WindowStart/WindowEnd are what was actually queried, echoed rather
	// than left to the client to recompute.
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`

	// PeriodSeconds is the bucket the provider USED, which may be
	// coarser than the one requested — CloudWatch stops keeping fine
	// resolution as data ages and answers a too-fine query with silence
	// rather than an error. AdjustReason says when that happened.
	PeriodSeconds int    `json:"period_seconds"`
	Stat          string `json:"stat,omitempty"`
	Adjusted      bool   `json:"adjusted"`
	AdjustReason  string `json:"adjust_reason,omitempty"`

	// Series is never null. An absent metric is PRESENT here with
	// `absent: true` and a reason, so the panel can say "never emitted"
	// instead of drawing a flat line at zero.
	Series []telemetry.Series `json:"series"`

	Capped    bool   `json:"capped"`
	CapReason string `json:"cap_reason,omitempty"`
	Calls     int    `json:"calls"`

	// Queried and Warnings are never null.
	Queried  []string `json:"queried"`
	Warnings []string `json:"warnings"`

	// Unavailable explains why there is nothing to show when the reason
	// is something other than "the workload was quiet". Non-empty means
	// Series is empty BY CONSTRUCTION.
	Unavailable string `json:"unavailable,omitempty"`

	// Inferred echoes the service record: these handles were guessed
	// from naming conventions rather than published by the plugin that
	// owns the resource, so a resolution that matched nothing may be a
	// wrong guess rather than an idle service.
	Inferred bool `json:"inferred"`
}

// Metrics serves `GET /api/v1/services/{stack}/metrics`.
//
// Query parameters:
//
//	minutes window length, counted back from now (default 180)
//	start   RFC3339 window start (overrides minutes)
//	end     RFC3339 window end   (default now)
//	period  aggregation bucket in seconds (default: chosen from the window)
//	stat    override the per-metric statistic (default: per-metric)
//	names   comma-separated metric names (default: the provider's
//	        catalogue for the service's namespace)
func (c *MetricsConfig) Metrics(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	// Operator-only, like every other read: a tenant's own `register`
	// token must not read another tenant's traffic numbers.
	if !requireOperator(w, r) {
		return
	}
	stack := pathParam(r, "stack")
	if stack == "" {
		writeErr(w, http.StatusBadRequest, "stack path param required")
		return
	}
	rec, found, err := c.Registry.Get(r.Context(), stack)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "service not registered")
		return
	}
	// Re-derive inferred handles from this build's conventions — see
	// registry.WithDerivedSources. A wrong convention is then fixed by
	// deploying observatory, not by redeploying every tenant.
	rec = registry.WithDerivedSources(rec)

	start, end, err := c.window(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	period, err := c.period(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	out := metricsResponse{
		Stack:       stack,
		WindowStart: start.Format(time.RFC3339),
		WindowEnd:   end.Format(time.RFC3339),
		Series:      []telemetry.Series{},
		Queried:     []string{},
		Warnings:    []string{},
		Inferred:    rec.Inferred,
	}

	provider, err := c.Providers.For(rec.Cloud)
	if err != nil {
		out.Unavailable = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Provider = provider.Name()
	if !provider.Capabilities().Metrics {
		out.Unavailable = provider.Name() + " cannot read metrics in this build"
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(rec.Sources) == 0 {
		out.Unavailable = "no telemetry handles are recorded for this service — its template published none and none could be inferred, so there is nothing to query"
		writeJSON(w, http.StatusOK, out)
		return
	}

	res, err := provider.Metrics(r.Context(), telemetry.MetricQuery{
		Sources:   rec.Sources,
		Names:     splitNames(r.URL.Query().Get("names")),
		Start:     start,
		End:       end,
		Period:    period,
		Stat:      r.URL.Query().Get("stat"),
		MaxSeries: c.MaxSeries,
	})
	// Carry whatever the provider collected even on the error paths: the
	// targets it tried and the period it settled on are the useful part
	// of a failed query.
	out.Queried = nonNilStrings(res.Queried)
	out.Warnings = nonNilStrings(res.Warnings)
	out.PeriodSeconds = res.PeriodSeconds
	out.Stat = res.Stat
	out.Adjusted = res.Adjusted
	out.AdjustReason = res.AdjustReason
	out.Calls = res.Calls

	if err != nil {
		// An UnavailableError is a reason, not a fault. A red error box
		// for "this workload has never been invoked" teaches operators
		// to ignore red error boxes.
		var unav *telemetry.UnavailableError
		if errors.As(err, &unav) {
			out.Unavailable = unav.Reason
			writeJSON(w, http.StatusOK, out)
			return
		}
		slog.Warn("metric query failed", "stack", stack, "cloud", rec.Cloud, "err", err)
		writeErr(w, http.StatusBadGateway, "metric query failed: "+err.Error())
		return
	}

	out.Series = res.Series
	if out.Series == nil {
		out.Series = []telemetry.Series{}
	}
	for i := range out.Series {
		if out.Series[i].Points == nil {
			out.Series[i].Points = []telemetry.Point{}
		}
	}
	out.Capped = res.Capped
	out.CapReason = res.CapReason
	writeJSON(w, http.StatusOK, out)
}

// window resolves the charted period from the request. Same parameters
// as the log window, different defaults — see DefaultWindow.
func (c *MetricsConfig) window(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	now := c.now()

	defWindow := c.DefaultWindow
	if defWindow <= 0 {
		defWindow = metricsDefaultWindow
	}
	maxWindow := c.MaxWindow
	if maxWindow <= 0 {
		maxWindow = metricsMaxWindow
	}

	end := now
	if raw := q.Get("end"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("end must be an RFC3339 timestamp")
		}
		end = t.UTC()
	}

	var start time.Time
	switch {
	case q.Get("start") != "":
		t, err := time.Parse(time.RFC3339, q.Get("start"))
		if err != nil {
			return time.Time{}, time.Time{}, errors.New("start must be an RFC3339 timestamp")
		}
		start = t.UTC()
	case q.Get("minutes") != "":
		n, err := strconv.Atoi(q.Get("minutes"))
		if err != nil || n <= 0 {
			return time.Time{}, time.Time{}, errors.New("minutes must be a positive integer")
		}
		start = end.Add(-time.Duration(n) * time.Minute)
	default:
		start = end.Add(-defWindow)
	}

	if !start.Before(end) {
		return time.Time{}, time.Time{}, errors.New("the window is empty: start must be before end")
	}
	if end.Sub(start) > maxWindow {
		return time.Time{}, time.Time{}, errors.New(
			"the window is wider than this deployment allows (" + maxWindow.String() + ") — narrow it",
		)
	}
	return start, end, nil
}

// period reads the requested bucket. Zero means "let the provider
// choose from the window", which is the normal case: the provider also
// knows its cloud's retention ladder, and a client-chosen period is how
// a chart ends up empty.
func (c *MetricsConfig) period(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("period")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("period must be a positive number of seconds")
	}
	return time.Duration(n) * time.Second, nil
}

// splitNames parses `names=a,b,c`. Empty yields nil, which the provider
// reads as "your catalogue" rather than as "no metrics".
func splitNames(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
