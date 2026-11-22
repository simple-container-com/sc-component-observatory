// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package awsprov

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwmtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
)

// SourceKindMetrics is the TelemetrySource.Kind this file serves.
const SourceKindMetrics = registry.KindCloudWatchMetrics

// dimPrefixSuffix marks a dimension whose recorded value is a PREFIX of
// the real one: `FunctionNamePrefix: "forge-conductor--staging-callback"`
// means dimension `FunctionName` starts with that string.
//
// Same reason the log handle used to be a prefix — SC appends a Pulumi
// suffix to the function name and the suffix is not derivable outside
// the deploy that created it. Unlike log groups there is no
// `DescribeLogGroups` equivalent that takes a prefix, so resolution
// goes through ListMetrics and filters client-side.
const dimPrefixSuffix = "Prefix"

// cwmAPI is the slice of the CloudWatch (metrics) API this provider
// uses. An interface for the same reason cwAPI is one: the period
// arithmetic, the prefix resolution and the absent-vs-empty decision
// are exactly the parts a live smoke cannot isolate, and all three are
// easy to get subtly wrong.
type cwmAPI interface {
	ListMetrics(ctx context.Context, in *cloudwatch.ListMetricsInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error)
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// metricDef is one chartable metric: its name, the statistic that makes
// sense for it, and the unit to label the axis with.
//
// The statistic is part of the DEFINITION, not of the request. Sum is
// the only correct aggregation for Invocations and is meaningless for
// Duration; a single request-wide `stat=Sum` would silently turn a
// latency chart into a nonsense one. CloudWatch itself will happily
// compute it.
type metricDef struct {
	Name string
	Stat string
	Unit string
}

// catalogues are the default metrics per namespace — what the SPA gets
// when it asks for none, which is the normal case.
//
// Deliberately short. The point of a service page is the handful of
// numbers that answer "is it healthy and is it being used"; a namespace
// dump is both unbounded and unordered, and the SPA has no way to
// explain a metric it did not ask for.
//
// This table is the ONLY place in the component that knows an AWS
// metric name. The SPA renders whatever series it is handed, so adding
// YC Monitoring or `metrics.k8s.io` later needs a table like this one
// and no frontend change.
var catalogues = map[string][]metricDef{
	"AWS/Lambda": {
		{Name: "Invocations", Stat: "Sum", Unit: "Count"},
		{Name: "Errors", Stat: "Sum", Unit: "Count"},
		{Name: "Throttles", Stat: "Sum", Unit: "Count"},
		{Name: "Duration", Stat: "Average", Unit: "Milliseconds"},
		{Name: "ConcurrentExecutions", Stat: "Maximum", Unit: "Count"},
	},
	"AWS/ECS": {
		{Name: "CPUUtilization", Stat: "Average", Unit: "Percent"},
		{Name: "MemoryUtilization", Stat: "Average", Unit: "Percent"},
	},
	"AWS/ApplicationELB": {
		{Name: "RequestCount", Stat: "Sum", Unit: "Count"},
		{Name: "TargetResponseTime", Stat: "Average", Unit: "Seconds"},
		{Name: "HTTPCode_Target_5XX_Count", Stat: "Sum", Unit: "Count"},
	},
}

// knownStats lets a caller-named metric outside the catalogue still get
// a sensible statistic, keyed on how AWS names things: a `*Count`,
// `*Invocations` or `Errors` metric is a counter, everything else is
// averaged.
func statFor(namespace, name, override string) (stat, unit string) {
	if defs, ok := catalogues[namespace]; ok {
		for _, d := range defs {
			if strings.EqualFold(d.Name, name) {
				if override != "" {
					return override, d.Unit
				}
				return d.Stat, d.Unit
			}
		}
	}
	if override != "" {
		return override, ""
	}
	switch {
	case strings.HasSuffix(name, "Count"),
		strings.HasSuffix(name, "Errors"),
		strings.HasSuffix(name, "Invocations"),
		strings.HasSuffix(name, "Throttles"):
		return "Sum", "Count"
	default:
		return "Average", ""
	}
}

// Metric-read bounds. Like the log budget, these are hard stops rather
// than hints — and like it, they are set so that capping is rare rather
// than impossible, since a dashboard that silently widens its own
// limits is the thing the risk register warns about.
const (
	// defaultMaxSeries bounds the fan-out. One handle resolving to three
	// functions × five catalogue metrics is fifteen series, which is
	// already more than a page can usefully draw.
	defaultMaxSeries = 24

	// targetPoints is the resolution the auto-period aims for: enough to
	// see a shape, few enough that twenty series fit a Function URL's
	// response cap.
	targetPoints = 180

	// maxPoints is the hard ceiling per series. A caller that asks for a
	// 48-hour window at 60s would otherwise get 2880 points per series.
	maxPoints = 720

	// listMetricsPages bounds discovery. 500 metrics per page, so two
	// pages cover a namespace with a thousand entries; a prefix matching
	// more than that is too broad to chart.
	listMetricsPages = 2

	// getMetricDataCalls bounds pagination of the read itself.
	getMetricDataCalls = 4

	// metricCacheTTL reuses a ListMetrics result. Short for the same
	// reason the log-group cache is: a redeploy creates a function with
	// a new Pulumi suffix, and charting the PREVIOUS deployment's
	// metrics would be a confident answer about the wrong resource.
	metricCacheTTL = 2 * time.Minute
)

// periodLadder is the set of periods the auto-chooser picks from. Every
// value above 60 is a multiple of 60, which CloudWatch requires.
var periodLadder = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
	time.Hour,
	3 * time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

// minPeriodForAge is CloudWatch's retention ladder, and the reason this
// whole file reports its period back to the caller.
//
// CloudWatch keeps 1-minute points for 15 days, 5-minute for 63 days
// and 1-hour for 455 days. Ask for a FINER period than the window's age
// allows and the call succeeds with an empty `Values` array — no error,
// no message. That renders as a flat, empty chart over a window the
// panel header says has data, which is exactly the silent degradation
// this component exists to prevent.
//
// The floor never goes below one minute even inside the 3-hour
// high-resolution window: sub-minute resolution exists only for custom
// metrics published with `StorageResolution=1`, and every
// AWS-published namespace emits at 60s. A 10-second period over
// 1-minute data returns five empty buckets out of every six, which
// draws as a gappy sawtooth and reads as dropped traffic.
//
// Returns 0 when nothing is retained at any resolution.
func minPeriodForAge(age time.Duration) time.Duration {
	switch {
	case age <= 15*24*time.Hour:
		return time.Minute
	case age <= 63*24*time.Hour:
		return 5 * time.Minute
	case age <= 455*24*time.Hour:
		return time.Hour
	default:
		return 0
	}
}

// choosePeriod resolves the aggregation bucket and says so when the
// resolved one is not the requested one.
//
// Raising rather than refusing is the right call here, unlike the log
// window (which is refused rather than clamped): a window the operator
// picked is the question being asked, while the period is only HOW
// finely it is answered. Coarsening it still answers the question — as
// long as the answer says it was coarsened, which is what the returned
// reason is for.
func choosePeriod(start, end, now time.Time, requested time.Duration) (time.Duration, string, error) {
	window := end.Sub(start)
	floor := minPeriodForAge(now.Sub(start))
	if floor == 0 {
		return 0, "", &telemetry.UnavailableError{Reason: fmt.Sprintf(
			"this window starts %d days ago and CloudWatch retains no metric data beyond 455 days",
			int(now.Sub(start).Hours()/24),
		)}
	}

	var notes []string
	p := requested
	if p <= 0 {
		p = autoPeriod(window)
	} else {
		p = snapPeriod(p)
	}
	if p < floor {
		notes = append(notes, fmt.Sprintf(
			"the period was raised from %s to %s because CloudWatch keeps only %s resolution for data this old (the window starts %s ago) — a finer period would have returned no points at all, with no error",
			short(requested), short(floor), short(floor), short(now.Sub(start)),
		))
		p = floor
	}
	if n := int(window / p); n > maxPoints {
		coarser := snapPeriod(window / maxPoints)
		notes = append(notes, fmt.Sprintf(
			"the period was raised to %s so the window fits within %d points per series (at %s it would have been %d)",
			short(coarser), maxPoints, short(p), n,
		))
		p = coarser
	}
	return p, strings.Join(notes, "; "), nil
}

// autoPeriod picks the finest ladder step that keeps the window near
// targetPoints.
func autoPeriod(window time.Duration) time.Duration {
	want := window / targetPoints
	for _, p := range periodLadder {
		if p >= want {
			return p
		}
	}
	return periodLadder[len(periodLadder)-1]
}

// snapPeriod rounds a requested period UP to something CloudWatch
// accepts: at least 60s, and a multiple of 60.
//
// Up, not to-nearest: rounding down can land under the retention floor
// and produce the empty result this file is built to avoid.
func snapPeriod(d time.Duration) time.Duration {
	if d <= time.Minute {
		return time.Minute
	}
	mins := (d + time.Minute - 1) / time.Minute
	return mins * time.Minute
}

// short formats a duration the way an operator reads one.
func short(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// metricTarget is one concrete dimension set to chart, in one region.
type metricTarget struct {
	region    string
	namespace string
	dims      map[string]string

	// label is what the chart legend says — the resolved resource name
	// when a prefix handle produced several targets.
	label string

	// from is the handle that produced it, kept so a surprising target
	// stays attributable to the derivation that guessed it.
	from string

	// have is the metric names CloudWatch LISTS for these dimensions.
	// The reason ListMetrics is called at all: without it, a metric that
	// does not exist for this workload and a metric that exists and
	// reported nothing both come back as an empty array.
	have map[string]bool
}

// slot is one series being assembled: the result so far, the target it
// belongs to, and the statistic to read it with. Pointer-shared so the
// per-region fetch can fill it in place while the response order stays
// the order the fan-out was built in.
type slot struct {
	series telemetry.Series
	target metricTarget
	stat   string
}

// Metrics reads one window of series for a service's metric handles.
func (p *Provider) Metrics(ctx context.Context, q telemetry.MetricQuery) (telemetry.MetricResult, error) {
	res := telemetry.MetricResult{
		Series:   []telemetry.Series{},
		Queried:  []string{},
		Warnings: []string{},
	}
	if q.Start.IsZero() || q.End.IsZero() || !q.Start.Before(q.End) {
		return res, &telemetry.UnavailableError{Reason: "the metric query carries no usable time window"}
	}

	period, adjust, err := choosePeriod(q.Start, q.End, p.now(), q.Period)
	if err != nil {
		return res, err
	}
	res.PeriodSeconds = int(period.Seconds())
	res.Stat = q.Stat
	if adjust != "" {
		res.Adjusted = true
		res.AdjustReason = adjust
	}

	maxSeries := q.MaxSeries
	if maxSeries <= 0 {
		maxSeries = defaultMaxSeries
	}

	targets, warns, calls := p.resolveMetricTargets(ctx, q.Sources)
	res.Warnings = append(res.Warnings, warns...)
	res.Calls += calls
	if len(targets) == 0 {
		if len(res.Warnings) > 0 {
			// Resolution ran and matched nothing. Identical on the wire to
			// a workload with no traffic unless it is said out loud — the
			// same trap the log prefix fell into on 2026-11-22.
			return res, &telemetry.UnavailableError{
				Reason: "no metric dimensions could be resolved for this service: " + strings.Join(res.Warnings, "; "),
			}
		}
		return res, &telemetry.UnavailableError{
			Reason: "this service has no CloudWatch metric handles recorded",
		}
	}

	// Build the fan-out: every (target × wanted metric) pair. Absent
	// ones are answered from the listing without spending a read.
	var (
		fetch   []*slot
		absent  []telemetry.Series
		skipped int
	)
	// Metric-major within a namespace, target-minor. The order survives
	// into the response, and it decides what a CAPPED read loses: this
	// way the cap drops the last metrics for every target, rather than
	// dropping the last target entirely and leaving its absence to be
	// noticed in the legend.
	nsOrder, byNamespace := groupByNamespace(targets)
	for _, t := range targets {
		res.Queried = append(res.Queried, t.label)
	}
	for _, ns := range nsOrder {
		for _, def := range defsFor(ns, q.Names, q.Stat) {
			for _, t := range byNamespace[ns] {
				s := telemetry.Series{
					Name:   def.Name,
					Unit:   def.Unit,
					Stat:   def.Stat,
					Label:  def.Name,
					Dims:   t.dims,
					Points: []telemetry.Point{},
				}
				if len(targets) > 1 {
					s.Label = def.Name + " · " + t.label
				}
				if !t.have[def.Name] {
					s.Absent = true
					s.Reason = fmt.Sprintf(
						"CloudWatch lists no %s metric for %s — this workload has never emitted it, so there is no data to be missing",
						def.Name, t.label,
					)
					absent = append(absent, s)
					continue
				}
				if len(fetch) >= maxSeries {
					skipped++
					continue
				}
				fetch = append(fetch, &slot{series: s, target: t, stat: def.Stat})
			}
		}
	}
	if skipped > 0 {
		res.Capped = true
		res.CapReason = fmt.Sprintf(
			"%d of %d series were not read: this request charts at most %d, and the handles for this service resolved to %d targets — ask for fewer metrics with `names=`, or chart one target at a time",
			skipped, len(fetch)+skipped, maxSeries, len(targets),
		)
	}

	// One GetMetricData batch per region: the API is regional, and a
	// multi-region service is normal.
	byRegion := map[string][]*slot{}
	for _, s := range fetch {
		byRegion[s.target.region] = append(byRegion[s.target.region], s)
	}
	regions := make([]string, 0, len(byRegion))
	for r := range byRegion {
		regions = append(regions, r)
	}
	sort.Strings(regions)

	var failed, attempted int
	for _, region := range regions {
		attempted++
		calls, warns, err := p.fetchSeries(ctx, region, byRegion[region], q.Start, q.End, period)
		res.Calls += calls
		res.Warnings = append(res.Warnings, warns...)
		if err != nil {
			failed++
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %s", region, errText(err)))
		}
	}
	// Every region failed: a fault, not a quiet service. Surfaced as an
	// error so it lands in a red box rather than as empty charts.
	if attempted > 0 && failed == attempted {
		return res, fmt.Errorf("every metric read failed: %s", strings.Join(res.Warnings, "; "))
	}

	for _, s := range fetch {
		res.Series = append(res.Series, s.series)
	}
	res.Series = append(res.Series, absent...)
	return res, nil
}

// groupByNamespace buckets targets by their namespace, keeping the
// namespaces in a stable order so the chart list does not reshuffle
// between polls.
func groupByNamespace(targets []metricTarget) ([]string, map[string][]metricTarget) {
	out := map[string][]metricTarget{}
	var order []string
	for _, t := range targets {
		if _, ok := out[t.namespace]; !ok {
			order = append(order, t.namespace)
		}
		out[t.namespace] = append(out[t.namespace], t)
	}
	sort.Strings(order)
	return order, out
}

// defsFor is the metric list for one namespace: the caller's names when
// it named any, the namespace catalogue otherwise.
func defsFor(namespace string, names []string, statOverride string) []metricDef {
	if len(names) == 0 {
		defs := catalogues[namespace]
		if statOverride == "" {
			return defs
		}
		out := make([]metricDef, 0, len(defs))
		for _, d := range defs {
			d.Stat = statOverride
			out = append(out, d)
		}
		return out
	}
	out := make([]metricDef, 0, len(names))
	for _, n := range names {
		stat, unit := statFor(namespace, n, statOverride)
		out = append(out, metricDef{Name: n, Stat: stat, Unit: unit})
	}
	return out
}

// fetchSeries fills one region's slots in place.
func (p *Provider) fetchSeries(
	ctx context.Context, region string, slots []*slot,
	start, end time.Time, period time.Duration,
) (int, []string, error) {
	if len(slots) == 0 {
		return 0, nil, nil
	}
	cl, err := p.metricClient(ctx, region)
	if err != nil {
		return 0, nil, err
	}

	byID := make(map[string]*slot, len(slots))
	queries := make([]cwmtypes.MetricDataQuery, 0, len(slots))
	for i, s := range slots {
		// The id must match ^[a-z][a-zA-Z0-9_]*$ — not the metric name,
		// which contains neither a legal first character nor legal
		// punctuation for several AWS metrics.
		id := fmt.Sprintf("m%d", i)
		byID[id] = s
		queries = append(queries, cwmtypes.MetricDataQuery{
			Id:         aws.String(id),
			ReturnData: aws.Bool(true),
			MetricStat: &cwmtypes.MetricStat{
				Metric: &cwmtypes.Metric{
					Namespace:  aws.String(s.target.namespace),
					MetricName: aws.String(s.series.Name),
					Dimensions: dimensionsOf(s.target.dims),
				},
				Period: aws.Int32(int32(period.Seconds())),
				Stat:   aws.String(s.stat),
			},
		})
	}

	var (
		warns []string
		calls int
		token *string
	)
	for i := 0; i < getMetricDataCalls; i++ {
		out, err := cl.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
			StartTime:         aws.Time(start),
			EndTime:           aws.Time(end),
			MetricDataQueries: queries,
			// Ascending so Points arrive in the order Series documents
			// them in, and so a truncated read loses the OLDEST part of
			// the window rather than the newest.
			ScanBy:    cwmtypes.ScanByTimestampAscending,
			NextToken: token,
		})
		calls++
		if err != nil {
			return calls, warns, err
		}
		for _, m := range out.Messages {
			// Request-level messages. Rare, and the one that matters
			// ("Invalid period over this time range") is precisely the
			// failure choosePeriod exists to prevent — so if it ever
			// appears, it must appear in the UI and not only here.
			warns = append(warns, fmt.Sprintf("CloudWatch: %s", aws.ToString(m.Value)))
		}
		for _, r := range out.MetricDataResults {
			s, ok := byID[aws.ToString(r.Id)]
			if !ok {
				continue
			}
			for j := range r.Timestamps {
				if j >= len(r.Values) {
					break
				}
				s.series.Points = append(s.series.Points, telemetry.Point{
					At:    r.Timestamps[j].UTC(),
					Value: r.Values[j],
				})
			}
			for _, m := range r.Messages {
				warns = append(warns, fmt.Sprintf("%s: %s", s.series.Label, aws.ToString(m.Value)))
			}
			if r.StatusCode == cwmtypes.StatusCodePartialData {
				s.series.Reason = "CloudWatch returned only part of this window; it may still be aggregating the most recent points"
			}
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		token = out.NextToken
	}

	// A series that exists but reported nothing in the window gets a
	// reason too. It is NOT Absent — the metric is real and the quiet is
	// the answer — but an empty chart with no caption reads as a broken
	// panel.
	for _, s := range slots {
		if len(s.series.Points) == 0 && s.series.Reason == "" {
			s.series.Reason = "this metric exists but reported no data points in the selected window"
		}
	}
	return calls, warns, nil
}

func dimensionsOf(dims map[string]string) []cwmtypes.Dimension {
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]cwmtypes.Dimension, 0, len(keys))
	for _, k := range keys {
		out = append(out, cwmtypes.Dimension{Name: aws.String(k), Value: aws.String(dims[k])})
	}
	return out
}

// resolveMetricTargets turns handles into concrete dimension sets,
// resolving the prefix dimensions the inference fallback produces.
//
// Warnings rather than errors per handle, for the same reason
// resolveTargets does it: one unresolvable handle must not sink the
// others, and must also not vanish.
func (p *Provider) resolveMetricTargets(ctx context.Context, sources []registry.TelemetrySource) ([]metricTarget, []string, int) {
	var (
		targets []metricTarget
		warns   []string
		calls   int
		seen    = map[string]bool{}
	)
	for _, s := range sources {
		if s.Kind != SourceKindMetrics {
			continue
		}
		region := s.Region
		if region == "" {
			region = p.cfg.DefaultRegion
		}
		if s.Ref == "" {
			warns = append(warns, "a recorded metric handle has no namespace")
			continue
		}

		filters, prefixes := dimFilters(s.Dims)
		if len(filters) == 0 {
			warns = append(warns, fmt.Sprintf(
				"the metric handle for %s names no dimensions, so it cannot be narrowed to this service", s.Ref,
			))
			continue
		}

		found, n, err := p.listMetrics(ctx, region, s.Ref, filters)
		calls += n
		if err != nil {
			warns = append(warns, fmt.Sprintf("listing metrics in %s: %s", s.Ref, errText(err)))
			continue
		}

		matched := 0
		for _, e := range found {
			if !matchesPrefixes(e.dims, prefixes) {
				continue
			}
			matched++
			key := region + "|" + s.Ref + "|" + dimKey(e.dims)
			if seen[key] {
				continue
			}
			seen[key] = true
			targets = append(targets, metricTarget{
				region:    region,
				namespace: s.Ref,
				dims:      e.dims,
				label:     labelOf(e.dims),
				from:      s.Ref,
				have:      e.names,
			})
		}
		if matched == 0 {
			// The single most important message in this file, and the
			// exact shape of the log-prefix bug: a guess that matches
			// nothing renders as a service with no traffic.
			warns = append(warns, fmt.Sprintf(
				"no metric in %s (%s) matches %s — this handle was derived from naming conventions, so the guess may be wrong, or the workload may never have been invoked",
				s.Ref, region, describeFilters(filters, prefixes),
			))
		}
	}
	// Stable order so the chart list does not reshuffle between polls.
	sort.Slice(targets, func(i, j int) bool { return targets[i].label < targets[j].label })
	return targets, warns, calls
}

// dimFilters splits a handle's recorded dimensions into a ListMetrics
// filter and the client-side prefix checks.
//
// A `<Name>Prefix` key becomes a NAME-ONLY filter (match the dimension
// with any value) plus a prefix to check locally, because ListMetrics
// has no prefix operator. The `match` marker itself is not a dimension.
func dimFilters(dims map[string]string) ([]cwmtypes.DimensionFilter, map[string]string) {
	var (
		filters  []cwmtypes.DimensionFilter
		prefixes = map[string]string{}
		keys     = make([]string, 0, len(dims))
	)
	for k := range dims {
		if k == DimMatch {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if base := strings.TrimSuffix(k, dimPrefixSuffix); base != k && base != "" {
			prefixes[base] = dims[k]
			filters = append(filters, cwmtypes.DimensionFilter{Name: aws.String(base)})
			continue
		}
		filters = append(filters, cwmtypes.DimensionFilter{Name: aws.String(k), Value: aws.String(dims[k])})
	}
	return filters, prefixes
}

func matchesPrefixes(dims, prefixes map[string]string) bool {
	for k, want := range prefixes {
		if !strings.HasPrefix(dims[k], want) {
			return false
		}
	}
	return true
}

func describeFilters(filters []cwmtypes.DimensionFilter, prefixes map[string]string) string {
	parts := make([]string, 0, len(filters))
	for _, f := range filters {
		n := aws.ToString(f.Name)
		if pre, ok := prefixes[n]; ok {
			parts = append(parts, fmt.Sprintf("%s starting with %q", n, pre))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%q", n, aws.ToString(f.Value)))
	}
	return strings.Join(parts, " and ")
}

// labelOf names a target from its dimensions: the most specific value,
// which for every namespace in the catalogue is the resource's own name.
func labelOf(dims map[string]string) string {
	for _, k := range []string{"FunctionName", "ServiceName", "ClusterName", "LoadBalancer", "TableName"} {
		if v := dims[k]; v != "" {
			return v
		}
	}
	keys := make([]string, 0, len(dims))
	for k := range dims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+dims[k])
	}
	return strings.Join(parts, ",")
}

func dimKey(dims map[string]string) string { return labelOf(dims) }

// listedTarget is one dimension set and the metric names found for it.
type listedTarget struct {
	dims  map[string]string
	names map[string]bool
}

// listMetrics discovers which metrics exist for a filtered namespace,
// grouped by dimension set.
//
// Only dimension sets whose keys EXACTLY match the filter are kept.
// AWS/Lambda publishes the same metric names under `{FunctionName}`,
// `{FunctionName, Resource}` and `{FunctionName, ExecutedVersion}`, and
// accepting all three would chart one function three times with three
// different totals.
func (p *Provider) listMetrics(ctx context.Context, region, namespace string, filters []cwmtypes.DimensionFilter) ([]listedTarget, int, error) {
	cacheKey := region + "|" + namespace + "|" + describeFilters(filters, nil)
	p.mu.Lock()
	if e, ok := p.metrics[cacheKey]; ok && p.now().Sub(e.at) < metricCacheTTL {
		p.mu.Unlock()
		return e.targets, 0, nil
	}
	p.mu.Unlock()

	cl, err := p.metricClient(ctx, region)
	if err != nil {
		return nil, 0, err
	}

	want := make(map[string]bool, len(filters))
	for _, f := range filters {
		want[aws.ToString(f.Name)] = true
	}

	var (
		calls int
		token *string
		byKey = map[string]*listedTarget{}
	)
	for i := 0; i < listMetricsPages; i++ {
		out, err := cl.ListMetrics(ctx, &cloudwatch.ListMetricsInput{
			Namespace:  aws.String(namespace),
			Dimensions: filters,
			NextToken:  token,
		})
		calls++
		if err != nil {
			return nil, calls, err
		}
		for _, m := range out.Metrics {
			dims := map[string]string{}
			ok := len(m.Dimensions) == len(want)
			for _, d := range m.Dimensions {
				n := aws.ToString(d.Name)
				if !want[n] {
					ok = false
					break
				}
				dims[n] = aws.ToString(d.Value)
			}
			if !ok {
				continue
			}
			key := dimKey(dims)
			t, exists := byKey[key]
			if !exists {
				t = &listedTarget{dims: dims, names: map[string]bool{}}
				byKey[key] = t
			}
			t.names[aws.ToString(m.MetricName)] = true
		}
		if out.NextToken == nil || *out.NextToken == "" {
			break
		}
		token = out.NextToken
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]listedTarget, 0, len(keys))
	for _, k := range keys {
		out = append(out, *byKey[k])
	}

	p.mu.Lock()
	p.metrics[cacheKey] = metricCacheEntry{targets: out, at: p.now()}
	p.mu.Unlock()
	return out, calls, nil
}

func (p *Provider) metricClient(ctx context.Context, region string) (cwmAPI, error) {
	if region == "" {
		region = p.cfg.DefaultRegion
	}
	p.mu.Lock()
	if c, ok := p.mclients[region]; ok {
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	c, err := p.metricClientFor(ctx, region)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.mclients[region] = c
	p.mu.Unlock()
	return c, nil
}

func (p *Provider) realMetricClient(ctx context.Context, region string) (cwmAPI, error) {
	loadOpts := []func(*config.LoadOptions) error{config.WithRegion(region)}
	if p.cfg.AccessKey != "" {
		loadOpts = append(loadOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(p.cfg.AccessKey, p.cfg.SecretKey, ""),
		))
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("awsprov: load aws config for %s: %w", region, err)
	}
	return cloudwatch.NewFromConfig(awsCfg), nil
}
