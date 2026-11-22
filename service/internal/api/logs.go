// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
)

// LogsConfig wires the log-query handler.
type LogsConfig struct {
	Registry  registry.Store
	Providers telemetry.Set

	// DefaultWindow is the lookback applied when the caller names no
	// window. Deliberately short: a wide default is how a dashboard
	// runs an expensive scan that nobody asked for, every time someone
	// opens a service page.
	DefaultWindow time.Duration

	// MaxWindow is the widest window accepted. Beyond it the request is
	// REFUSED rather than silently clamped — a clamped window returns
	// results from a different period than the one the UI is labelling,
	// which is the dashboard misreporting its own axis.
	MaxWindow time.Duration

	// DefaultLimit / MaxLimit bound one page.
	DefaultLimit int
	MaxLimit     int

	// Budget caps the scan. Zero fields fall back to the provider's own
	// defaults.
	Budget telemetry.Budget

	// Now is injected for tests.
	Now func() time.Time
}

// Fallbacks, used when the operator set no env override.
const (
	logsDefaultWindow = 15 * time.Minute
	logsMaxWindow     = 48 * time.Hour
	logsDefaultLimit  = 200
	logsMaxLimit      = 500
)

func (c *LogsConfig) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

// logsResponse is the wire shape.
//
// Everything about it is built around one rule: a reader must never be
// unable to tell "nothing matched" from "we could not look". So the
// window that was searched, the handles that were read, the scan that
// was spent, what stopped it, and every per-source failure are all on
// the response — not in a log line the operator will never see.
type logsResponse struct {
	Stack string `json:"stack"`

	// Provider is the cloud that served this, so a reader knows which
	// implementation's limits apply.
	Provider string `json:"provider,omitempty"`

	// WindowStart/WindowEnd are what was ACTUALLY searched, echoed back
	// rather than left to the client to recompute. A client that
	// renders its own idea of the window will eventually render a
	// different one than the server used.
	WindowStart string `json:"window_start"`
	WindowEnd   string `json:"window_end"`

	// Events is newest-first and never null.
	Events []telemetry.LogEvent `json:"events"`

	NextCursor string `json:"next_cursor,omitempty"`

	Capped    bool   `json:"capped"`
	CapReason string `json:"cap_reason,omitempty"`
	Scanned   int    `json:"scanned"`
	Calls     int    `json:"calls"`

	// Queried and Warnings are never null.
	Queried  []string `json:"queried"`
	Warnings []string `json:"warnings"`

	// Unavailable explains why there is nothing to show, when the
	// reason is something other than "the window was quiet". Non-empty
	// means Events is empty BY CONSTRUCTION — not coincidentally.
	Unavailable string `json:"unavailable,omitempty"`

	// Inferred echoes the service record: these handles were guessed
	// from naming conventions rather than published by the plugin that
	// owns the resource, so an empty result may be a wrong lookup.
	Inferred bool `json:"inferred"`

	// Filter is the pattern that was applied, echoed so a surprised
	// reader can see whether one was in effect.
	Filter string `json:"filter,omitempty"`
}

// Logs serves `GET /api/v1/services/{stack}/logs`.
//
// Query parameters:
//
//	q       provider-native filter pattern (CloudWatch filter syntax on AWS)
//	minutes window length, counted back from now (default 15)
//	start   RFC3339 window start (overrides minutes)
//	end     RFC3339 window end   (default now)
//	limit   events per page
//	cursor  continue OLDER than a previous page; send the SAME window
//	        parameters alongside it, because the cursor moves only the
//	        window's end and carries no window of its own
func (c *LogsConfig) Logs(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	// Operator-only, like every other read: a tenant's own `register`
	// token must not read another tenant's log output.
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

	start, end, err := c.window(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := c.limit(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	out := logsResponse{
		Stack:       stack,
		WindowStart: start.Format(time.RFC3339),
		WindowEnd:   end.Format(time.RFC3339),
		Events:      []telemetry.LogEvent{},
		Queried:     []string{},
		Warnings:    []string{},
		Inferred:    rec.Inferred,
		Filter:      r.URL.Query().Get("q"),
	}

	provider, err := c.Providers.For(rec.Cloud)
	if err != nil {
		out.Unavailable = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Provider = provider.Name()
	if !provider.Capabilities().Logs {
		out.Unavailable = provider.Name() + " cannot read logs in this build"
		writeJSON(w, http.StatusOK, out)
		return
	}
	if len(rec.Sources) == 0 {
		// Said here rather than left to the provider, because the
		// reason is different: there is no handle to query at all,
		// which is a registration-time fact and not a query failure.
		out.Unavailable = "no telemetry handles are recorded for this service — its template published none and none could be inferred, so there is nothing to query"
		writeJSON(w, http.StatusOK, out)
		return
	}

	page, err := provider.Logs(r.Context(), telemetry.LogQuery{
		Sources: rec.Sources,
		Filter:  out.Filter,
		Start:   start,
		End:     end,
		Limit:   limit,
		Cursor:  r.URL.Query().Get("cursor"),
		Budget:  c.Budget,
	})
	// Carry whatever the provider managed to collect even on the error
	// paths: the handles it tried and the calls it spent are the useful
	// part of a failed query.
	out.Queried = nonNilStrings(page.Queried)
	out.Warnings = nonNilStrings(page.Warnings)
	out.Scanned = page.Scanned
	out.Calls = page.Calls

	if err != nil {
		// An UnavailableError is a reason, not a fault — render it as
		// one. A red error box for "this service has no log handles"
		// teaches operators to ignore red error boxes.
		var unav *telemetry.UnavailableError
		if errors.As(err, &unav) {
			out.Unavailable = unav.Reason
			writeJSON(w, http.StatusOK, out)
			return
		}
		slog.Warn("log query failed", "stack", stack, "cloud", rec.Cloud, "err", err)
		writeErr(w, http.StatusBadGateway, "log query failed: "+err.Error())
		return
	}

	out.Events = page.Events
	if out.Events == nil {
		out.Events = []telemetry.LogEvent{}
	}
	out.NextCursor = page.NextCursor
	out.Capped = page.Capped
	out.CapReason = page.CapReason
	writeJSON(w, http.StatusOK, out)
}

// window resolves the searched period from the request.
func (c *LogsConfig) window(r *http.Request) (time.Time, time.Time, error) {
	q := r.URL.Query()
	now := c.now()

	defWindow := c.DefaultWindow
	if defWindow <= 0 {
		defWindow = logsDefaultWindow
	}
	maxWindow := c.MaxWindow
	if maxWindow <= 0 {
		maxWindow = logsMaxWindow
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
		// Refused, not clamped. See MaxWindow.
		return time.Time{}, time.Time{}, errors.New(
			"the window is wider than this deployment allows (" + maxWindow.String() +
				") — narrow it, or add a filter and query a specific period",
		)
	}
	return start, end, nil
}

func (c *LogsConfig) limit(r *http.Request) (int, error) {
	def, maxL := c.DefaultLimit, c.MaxLimit
	if def <= 0 {
		def = logsDefaultLimit
	}
	if maxL <= 0 {
		maxL = logsMaxLimit
	}
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return min(def, maxL), nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("limit must be a positive integer")
	}
	return min(n, maxL), nil
}

// nonNilStrings keeps every slice on the wire a real array. An absent
// field a client spreads with `[...x]` throws "x is not iterable" and
// blanks the page, with nothing naming the field — a failure mode this
// SPA has already paid for once.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
