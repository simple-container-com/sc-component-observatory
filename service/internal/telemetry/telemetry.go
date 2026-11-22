// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package telemetry is the cloud-agnostic read surface for logs,
// metrics and live workload state.
//
// One interface, one implementation per cloud, selected per service by
// the `cloud` field the registry recorded at deploy time. The SPA and
// the HTTP layer know nothing about CloudWatch, YC Monitoring or
// `metrics.k8s.io` — which is the whole point of the component: adding
// a cloud must not mean touching the dashboard.
//
// The rule every type here is shaped around: **absent is not empty.**
// A provider that is not configured, a capability that is not
// implemented, and a query that genuinely found nothing are three
// different answers, and an operator acts differently on each. So a
// caller never gets a bare empty slice — it gets a reason, or a
// typed error it can turn into one.
package telemetry

import (
	"context"
	"fmt"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// Capabilities says which reads a provider can actually serve.
//
// It exists so the dashboard can render "metrics are not implemented
// for cloud=yandex" instead of an empty chart. `Health() == nil` cannot
// carry that: a healthy provider with no metrics implementation and a
// healthy provider whose service emitted no metrics both answer "fine"
// and return nothing, and those are opposite situations.
type Capabilities struct {
	Logs      bool `json:"logs"`
	Metrics   bool `json:"metrics"`
	Workloads bool `json:"workloads"`
}

// Provider is one cloud's telemetry surface.
//
// Implementations must never DERIVE a resource identifier. Every query
// carries the handles the owning template plugin published (or, until
// slice 4, the flagged inference). The component that created a
// resource is the only thing that reliably knows its identifiers, and
// a guess that misses renders as an empty log view — which reads as a
// quiet service rather than a wrong lookup.
type Provider interface {
	// Name is the cloud token: "aws" | "yandex" | "kubernetes".
	Name() string

	// Capabilities is what this implementation can serve today.
	Capabilities() Capabilities

	// Logs returns one page of log events for a service's sources.
	Logs(ctx context.Context, q LogQuery) (LogPage, error)

	// Metrics returns time series for the requested metric names,
	// alongside the query that was ACTUALLY run — see MetricResult for
	// why the two can differ.
	Metrics(ctx context.Context, q MetricQuery) (MetricResult, error)

	// Workloads is what is RUNNING — replica counts, memory and CPU
	// allotments, concurrency limits. Separate from the declared
	// counts in the deploy config snapshot on purpose: showing one and
	// labelling it as the other is how a dashboard says a
	// scaled-to-zero service is fine.
	Workloads(ctx context.Context, q WorkloadQuery) ([]Workload, error)

	// Health is a cheap liveness probe for the credential and the
	// endpoint, so a provider that cannot authenticate is visible as
	// broken rather than as a fleet with no telemetry.
	Health(ctx context.Context) error
}

// LogQuery is one page request against a service's log handles.
//
// The window is always explicit — there is no "everything" query. An
// unbounded log read is a real bill on CloudWatch and a real timeout on
// a Lambda, and both failures arrive as an empty page.
type LogQuery struct {
	// Sources are the handles from the service record. The provider
	// filters for the kinds it serves and ignores the rest.
	Sources []registry.TelemetrySource

	// Filter is a provider-native pattern. Empty means no filtering.
	// Passed through rather than parsed: CloudWatch filter syntax, a YC
	// filter expression and a grep are not the same language, and a
	// lowest-common-denominator query language would be the worst of
	// the three.
	Filter string

	// Start and End bound the window. End is exclusive. Both are
	// always set by the HTTP layer, which applies the defaults — a
	// provider receiving a zero Start would have to invent a window,
	// and the invented one would not be the one the UI displayed.
	Start, End time.Time

	// Limit is the maximum number of events to return.
	Limit int

	// Cursor continues a previous page. Opaque to the caller and
	// PROVIDER-AGNOSTIC by construction (see Cursor) — it carries a
	// timestamp boundary, not a CloudWatch pagination token, so the
	// wire format does not leak one cloud's pagination model.
	Cursor string

	// Budget caps the work one request may do. Zero means the
	// provider's default.
	Budget Budget
}

// Budget is the scan cap. Every field is a hard stop, not a hint.
//
// Three separate limits because they bound three different failure
// modes, and any one of them alone leaves a hole: MaxEventsScanned
// bounds the CloudWatch bill, MaxCalls bounds the wall clock (a Lambda
// that exceeds its timeout returns no page at all, which looks like an
// outage), and MaxBytes bounds the response (a Function URL has a hard
// response cap and exceeding it fails the whole request, not the tail
// of it).
type Budget struct {
	MaxEventsScanned int
	MaxCalls         int
	MaxBytes         int
	// MaxMessageBytes truncates one oversized event rather than
	// letting it consume the whole byte budget. A 2 MB stack trace in
	// one log line is not hypothetical.
	MaxMessageBytes int
}

// LogEvent is one line.
type LogEvent struct {
	At      time.Time `json:"at"`
	Message string    `json:"message"`

	// Stream is the log stream / pod / container the line came from.
	Stream string `json:"stream,omitempty"`

	// Source is the handle that produced it, so a page merged from
	// several log groups stays attributable.
	Source string `json:"source,omitempty"`

	// ID is the provider's event id. Used for the cursor's
	// boundary de-duplication, and exposed so a UI can key a list
	// without index keys.
	ID string `json:"id,omitempty"`

	// Truncated marks a message cut to MaxMessageBytes.
	Truncated bool `json:"truncated,omitempty"`
}

// LogPage is one page of results plus everything needed to describe
// honestly what was and was not read.
type LogPage struct {
	// Events is newest-first and NEVER nil.
	Events []LogEvent `json:"events"`

	// NextCursor continues OLDER than this page. Empty means the
	// provider reached the start of the window.
	NextCursor string `json:"next_cursor,omitempty"`

	// Capped says a Budget limit stopped the scan before the window
	// was exhausted, and CapReason says which one and what that means
	// for the result. Without this a capped page is indistinguishable
	// from a quiet service.
	Capped    bool   `json:"capped"`
	CapReason string `json:"cap_reason,omitempty"`

	// Scanned and Calls are what the query cost. Surfaced because the
	// risk register calls for the scan being visible to the operator
	// who triggered it.
	Scanned int `json:"scanned"`
	Calls   int `json:"calls"`

	// Queried are the concrete handles actually read. A prefix handle
	// resolves to several real log groups, and "which groups did you
	// search" is the first question when a search comes back empty.
	Queried []string `json:"queried"`

	// Warnings are per-source failures that did not sink the query —
	// one of three log groups denied, say. Never nil. Promoting these
	// to an error would hide the two groups that did work; dropping
	// them would present a partial answer as complete.
	Warnings []string `json:"warnings"`
}

// MetricQuery asks for a service's series over a window.
//
// WorkloadQuery / Workload below are still declared-not-served (slice
// 3.5). They are here rather than in a later commit because the
// interface is the contract the SPA is written against, and discovering
// a missing field after two cloud implementations exist means changing
// both.
type MetricQuery struct {
	Sources []registry.TelemetrySource

	// Names selects metrics. EMPTY MEANS THE PROVIDER'S DEFAULT
	// CATALOGUE for whatever namespaces the sources name — not "all
	// metrics". "All" is unbounded (a shared ECS cluster publishes
	// hundreds) and, worse, unlabelled: the SPA has no way to order or
	// explain a list it did not ask for. The catalogue is per-namespace
	// and lives in the provider, so the SPA never learns a cloud's
	// metric names.
	Names []string

	Start, End time.Time

	// Period is the aggregation bucket. Zero means "choose one from the
	// window", which is the normal case — a caller that picks its own
	// period will eventually pick one the cloud refuses to serve.
	Period time.Duration

	// Stat overrides the per-metric default statistic. Normally empty:
	// Sum is right for Invocations and nonsense for Duration, so the
	// catalogue carries a statistic per metric and a caller that sets
	// one flat value here is usually asking for a wrong chart.
	Stat string

	// MaxSeries caps how many series one request may fetch. Zero means
	// the provider's default.
	MaxSeries int
}

// MetricResult is the series plus a description of the query that was
// actually run.
//
// The second half is not bookkeeping. Every cloud silently rewrites a
// metric query it cannot serve as asked — CloudWatch keeps 1-minute
// resolution for 15 days and 5-minute for 63, and a finer period over
// an older window returns an EMPTY RESULT rather than an error. A
// dashboard that echoed the period it asked for would then draw a blank
// chart labelled with a resolution the data never had. So the period
// that ran, the statistic that ran, and any adjustment are part of the
// answer.
type MetricResult struct {
	// Series is NEVER nil, and a requested metric always appears —
	// flagged Absent with a Reason when the cloud has no such metric
	// for this workload. Dropping it would make "this workload never
	// emitted Errors" look identical to "it emitted no errors".
	Series []Series `json:"series"`

	// PeriodSeconds and Stat are what RAN, not what was asked for.
	PeriodSeconds int    `json:"period_seconds"`
	Stat          string `json:"stat,omitempty"`

	// Queried names the concrete targets read — a prefix handle
	// resolves to real resource names, and "which function did you
	// chart" is the first question when a number looks wrong.
	Queried []string `json:"queried"`

	// Warnings are per-source failures that did not sink the query.
	// Never nil.
	Warnings []string `json:"warnings"`

	// Capped means MaxSeries stopped the fan-out before every
	// (metric × target) pair was fetched. CapReason says what was left
	// out, because a chart silently missing one of three functions is
	// the dashboard reporting a partial sum as a total.
	Capped    bool   `json:"capped"`
	CapReason string `json:"cap_reason,omitempty"`

	// Adjusted means the query that ran differs from the one asked for
	// — today always the period, raised to what the cloud still keeps
	// at that age. Reported out loud for the reason in the type
	// comment: the alternative is a correct-looking empty chart.
	Adjusted     bool   `json:"adjusted"`
	AdjustReason string `json:"adjust_reason,omitempty"`

	// Calls is what the query cost in API round trips, same contract as
	// LogPage.Calls.
	Calls int `json:"calls"`
}

// Series is one named time series. Points are ascending by time.
type Series struct {
	Name string `json:"name"`
	Unit string `json:"unit,omitempty"`

	// Stat is the aggregation this series was computed with. Per-series
	// rather than per-request because the right statistic is a property
	// of the metric: Sum for a counter, Average for a latency, Maximum
	// for a concurrency ceiling. A chart that does not say which one it
	// drew cannot be read.
	Stat string `json:"stat,omitempty"`

	// Label is the display name, including which target this series
	// belongs to when a handle resolved to several.
	Label string `json:"label,omitempty"`

	Dims   map[string]string `json:"dims,omitempty"`
	Points []Point           `json:"points"`

	// Absent distinguishes "this metric does not exist for this
	// workload" from "it exists and reported no data in the window" —
	// the distinction `ListMetrics` is called for (slice 3), and the
	// reason an empty chart is never an acceptable rendering.
	Absent bool   `json:"absent,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type Point struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

type WorkloadQuery struct {
	Sources []registry.TelemetrySource
	Stack   string
}

// Workload is cloud-shaped but not cloud-named, for the same reason the
// config differ's Compute bucket is: a Lambda's reserved concurrency,
// an ECS service's desiredCount/runningCount and a k8s Deployment's
// replicas/readyReplicas are one question asked three ways, and the SPA
// must not learn all three.
type Workload struct {
	Kind string `json:"kind"`
	Name string `json:"name"`

	// Desired is what the configuration asks for; Running is what the
	// cloud reports is actually up. Kept as a pair, and both nullable,
	// because a provider that can only answer one of them must say so
	// rather than letting the other read as zero.
	Desired *int `json:"desired,omitempty"`
	Running *int `json:"running,omitempty"`

	MemoryMB       *int              `json:"memory_mb,omitempty"`
	CPU            string            `json:"cpu,omitempty"`
	ConcurrencyCap *int              `json:"concurrency_cap,omitempty"`
	Extra          map[string]string `json:"extra,omitempty"`
}

// Set maps a cloud token to its provider.
//
// A nil or absent entry is a first-class answer, not a nil-pointer
// panic: most fleets are single-cloud, so most Sets have exactly one
// entry and every other cloud must resolve to a reason.
type Set map[string]Provider

// For returns the provider for a cloud, or an error that names what is
// missing. The error text is operator-facing — it lands in the
// dashboard's `unavailable` field verbatim.
func (s Set) For(cloud string) (Provider, error) {
	if cloud == "" {
		return nil, &UnavailableError{Reason: "this service's record has no cloud recorded, so no telemetry provider can be selected"}
	}
	p, ok := s[cloud]
	if !ok || p == nil {
		return nil, &UnavailableError{Reason: fmt.Sprintf("no telemetry provider is configured for cloud=%q — this deployment of observatory cannot read its telemetry", cloud)}
	}
	return p, nil
}

// Clouds lists the configured clouds, for the startup log.
func (s Set) Clouds() []string {
	out := make([]string, 0, len(s))
	for k, v := range s {
		if v != nil {
			out = append(out, k)
		}
	}
	return out
}

// UnavailableError is a NON-failure: the query could not be served, and
// the reason is something the operator can act on (wire up a
// credential, wait for a slice to land). The HTTP layer renders it as a
// 200 with an `unavailable` reason rather than an error, because a red
// error box for "metrics land in slice 3" trains operators to ignore
// red boxes.
type UnavailableError struct {
	Reason string
}

func (e *UnavailableError) Error() string { return e.Reason }

// Unsupported is the UnavailableError a provider returns for a
// capability it has not implemented. Separate constructor so the
// message is phrased identically everywhere.
func Unsupported(provider, capability string) error {
	return &UnavailableError{Reason: fmt.Sprintf("%s does not implement %s yet", provider, capability)}
}
