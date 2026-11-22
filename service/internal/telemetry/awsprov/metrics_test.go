// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package awsprov

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwmtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
)

// fakeCWM behaves like the CloudWatch metrics API for the parts that
// matter: ListMetrics honours the dimension filter and pages, and
// GetMetricData answers only for (metric, dims) pairs that were listed.
//
// Modelled rather than stubbed for the same reason fakeCW is: the three
// things most likely to be subtly wrong here — the prefix resolution,
// the exact-dimension-set rule that stops one Lambda being charted
// three times, and absent-vs-empty — are all invisible to a live smoke,
// which only ever shows whether SOME series came back.
type fakeCWM struct {
	// listed is the account's metric inventory: namespace → entries.
	listed map[string][]cwmtypes.Metric

	// values keyed by "metric|label" supplies points. A listed metric
	// with no entry here is the "exists but reported nothing" case.
	values map[string][]float64

	listPageSize int
	listErr      error
	getErr       error

	listCalls int
	getCalls  int

	// lastGet records the queries of the most recent GetMetricData, so a
	// test can assert the period and statistic that actually went out.
	lastGet []cwmtypes.MetricDataQuery
}

func (f *fakeCWM) ListMetrics(_ context.Context, in *cloudwatch.ListMetricsInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.ListMetricsOutput, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []cwmtypes.Metric
	for _, m := range f.listed[aws.ToString(in.Namespace)] {
		if !matchesFilter(m, in.Dimensions) {
			continue
		}
		out = append(out, m)
	}
	// Paginate deterministically from an offset encoded in the token.
	from := 0
	if in.NextToken != nil {
		from = len(*in.NextToken) // tests use a token of N characters
	}
	size := f.listPageSize
	if size <= 0 || from+size >= len(out) {
		return &cloudwatch.ListMetricsOutput{Metrics: out[min(from, len(out)):]}, nil
	}
	next := strings.Repeat("x", from+size)
	return &cloudwatch.ListMetricsOutput{Metrics: out[from : from+size], NextToken: &next}, nil
}

// matchesFilter implements ListMetrics' documented filter semantics: a
// filter with a Name only matches any value of that dimension, a filter
// with both must match exactly, and a metric must carry every filtered
// dimension.
func matchesFilter(m cwmtypes.Metric, filters []cwmtypes.DimensionFilter) bool {
	for _, f := range filters {
		found := false
		for _, d := range m.Dimensions {
			if aws.ToString(d.Name) != aws.ToString(f.Name) {
				continue
			}
			if f.Value != nil && aws.ToString(d.Value) != aws.ToString(f.Value) {
				continue
			}
			found = true
			break
		}
		if !found {
			return false
		}
	}
	return true
}

func (f *fakeCWM) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.lastGet = in.MetricDataQueries
	period := time.Duration(aws.ToInt32(in.MetricDataQueries[0].MetricStat.Period)) * time.Second

	var results []cwmtypes.MetricDataResult
	for _, q := range in.MetricDataQueries {
		name := aws.ToString(q.MetricStat.Metric.MetricName)
		label := ""
		for _, d := range q.MetricStat.Metric.Dimensions {
			label = aws.ToString(d.Value)
		}
		vals := f.values[name+"|"+label]
		r := cwmtypes.MetricDataResult{
			Id:         q.Id,
			StatusCode: cwmtypes.StatusCodeComplete,
		}
		for i, v := range vals {
			r.Timestamps = append(r.Timestamps, in.StartTime.Add(time.Duration(i)*period))
			r.Values = append(r.Values, v)
		}
		results = append(results, r)
	}
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: results}, nil
}

func newMetricProvider(t *testing.T, f *fakeCWM) *Provider {
	t.Helper()
	p, err := New(context.Background(), Config{
		DefaultRegion: "eu-central-1",
		Now:           func() time.Time { return base.Add(time.Hour) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.metricClientFor = func(context.Context, string) (cwmAPI, error) { return f, nil }
	return p
}

func lambdaMetric(fn string, name string, extra ...string) cwmtypes.Metric {
	dims := []cwmtypes.Dimension{{Name: aws.String("FunctionName"), Value: aws.String(fn)}}
	for i := 0; i+1 < len(extra); i += 2 {
		dims = append(dims, cwmtypes.Dimension{Name: aws.String(extra[i]), Value: aws.String(extra[i+1])})
	}
	return cwmtypes.Metric{
		Namespace:  aws.String("AWS/Lambda"),
		MetricName: aws.String(name),
		Dimensions: dims,
	}
}

func lambdaSource(stack string) registry.TelemetrySource {
	return registry.TelemetrySource{
		Kind:   registry.KindCloudWatchMetrics,
		Ref:    "AWS/Lambda",
		Region: "eu-central-1",
		Dims:   map[string]string{"match": "prefix", "FunctionNamePrefix": stack + "-callback"},
	}
}

func seriesByName(res telemetry.MetricResult, name string) (telemetry.Series, bool) {
	for _, s := range res.Series {
		if s.Name == name {
			return s, true
		}
	}
	return telemetry.Series{}, false
}

// TestMetricsResolvesPrefixDimension — the whole reason ListMetrics is
// called before GetMetricData: SC appends a Pulumi suffix to the
// function name, so the recorded handle is a prefix and the concrete
// dimension has to be discovered.
func TestMetricsResolvesPrefixDimension(t *testing.T) {
	f := &fakeCWM{
		listed: map[string][]cwmtypes.Metric{"AWS/Lambda": {
			lambdaMetric("forge-conductor--staging-callback-ab12cd3", "Invocations"),
			lambdaMetric("forge-conductor--staging-callback-ab12cd3", "Duration"),
			// Another tenant's function: must NOT be charted.
			lambdaMetric("forge-notifier--staging-callback-99ff00a", "Invocations"),
		}},
		values: map[string][]float64{
			"Invocations|forge-conductor--staging-callback-ab12cd3": {3, 0, 7},
			"Duration|forge-conductor--staging-callback-ab12cd3":    {12.5, 11, 40},
		},
	}
	p := newMetricProvider(t, f)

	res, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{lambdaSource("forge-conductor--staging")},
		Start:   base, End: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if len(res.Queried) != 1 || res.Queried[0] != "forge-conductor--staging-callback-ab12cd3" {
		t.Fatalf("Queried = %v, want only the matching function", res.Queried)
	}
	inv, ok := seriesByName(res, "Invocations")
	if !ok {
		t.Fatal("Invocations series missing")
	}
	if inv.Dims["FunctionName"] != "forge-conductor--staging-callback-ab12cd3" {
		t.Errorf("dims = %v, want the resolved function name", inv.Dims)
	}
	if len(inv.Points) != 3 || inv.Points[0].Value != 3 {
		t.Errorf("points = %v, want the three fake values", inv.Points)
	}
	// Ascending by time, which Series documents and the SPA relies on.
	for i := 1; i < len(inv.Points); i++ {
		if !inv.Points[i].At.After(inv.Points[i-1].At) {
			t.Fatalf("points are not ascending: %v", inv.Points)
		}
	}
	if inv.Stat != "Sum" {
		t.Errorf("Invocations stat = %q, want Sum — a counter averaged is a wrong chart", inv.Stat)
	}
	if d, _ := seriesByName(res, "Duration"); d.Stat != "Average" {
		t.Errorf("Duration stat = %q, want Average", d.Stat)
	}
}

// TestMetricsAbsentIsNotEmpty — the component's whole reason to exist.
// A metric the workload never emitted must come back flagged, not
// missing and not as a flat zero line.
func TestMetricsAbsentIsNotEmpty(t *testing.T) {
	f := &fakeCWM{
		listed: map[string][]cwmtypes.Metric{"AWS/Lambda": {
			lambdaMetric("svc-callback-1", "Invocations"),
			lambdaMetric("svc-callback-1", "Duration"),
			// No Errors and no Throttles: never emitted.
			// ConcurrentExecutions listed but with no data in the window.
			lambdaMetric("svc-callback-1", "ConcurrentExecutions"),
		}},
		values: map[string][]float64{"Invocations|svc-callback-1": {1}},
	}
	p := newMetricProvider(t, f)

	res, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{lambdaSource("svc")},
		Start:   base, End: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	// Every catalogue metric is present on the wire.
	for _, want := range []string{"Invocations", "Errors", "Throttles", "Duration", "ConcurrentExecutions"} {
		if _, ok := seriesByName(res, want); !ok {
			t.Fatalf("%s is missing from the response — an absent metric must be reported, not dropped", want)
		}
	}
	errs, _ := seriesByName(res, "Errors")
	if !errs.Absent {
		t.Error("Errors was never emitted and must be flagged Absent")
	}
	if !strings.Contains(errs.Reason, "never emitted") {
		t.Errorf("Errors reason = %q, want it to say the metric does not exist", errs.Reason)
	}
	if errs.Points == nil {
		t.Error("Points must be a real array even for an absent metric")
	}

	// Listed but silent: NOT absent, and still captioned.
	conc, _ := seriesByName(res, "ConcurrentExecutions")
	if conc.Absent {
		t.Error("a listed metric with no points in the window is not Absent — the metric is real")
	}
	if !strings.Contains(conc.Reason, "no data points") {
		t.Errorf("silent series reason = %q, want it to distinguish quiet from missing", conc.Reason)
	}
}

// TestMetricsOneSeriesPerFunction — AWS/Lambda publishes the same
// metric names under {FunctionName}, {FunctionName,Resource} and
// {FunctionName,ExecutedVersion}. Accepting all three charts one
// function three times with three different totals.
func TestMetricsOneSeriesPerFunction(t *testing.T) {
	f := &fakeCWM{
		listed: map[string][]cwmtypes.Metric{"AWS/Lambda": {
			lambdaMetric("svc-callback-1", "Invocations"),
			lambdaMetric("svc-callback-1", "Invocations", "Resource", "svc-callback-1"),
			lambdaMetric("svc-callback-1", "Invocations", "ExecutedVersion", "$LATEST"),
		}},
		values: map[string][]float64{"Invocations|svc-callback-1": {5}},
	}
	p := newMetricProvider(t, f)

	res, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{lambdaSource("svc")},
		Names:   []string{"Invocations"},
		Start:   base, End: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if len(res.Series) != 1 {
		t.Fatalf("got %d series, want exactly one per function: %+v", len(res.Series), res.Series)
	}
	if len(res.Queried) != 1 {
		t.Fatalf("Queried = %v, want one target", res.Queried)
	}
}

// TestMetricsPrefixMatchesNothingSaysSo — the exact shape of the log
// bug this component already paid for once: a derived handle that
// matches nothing renders as a service with no traffic unless the
// mismatch is named.
func TestMetricsPrefixMatchesNothingSaysSo(t *testing.T) {
	f := &fakeCWM{listed: map[string][]cwmtypes.Metric{"AWS/Lambda": {
		lambdaMetric("someone-else-callback-1", "Invocations"),
	}}}
	p := newMetricProvider(t, f)

	res, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{lambdaSource("svc")},
		Start:   base, End: base.Add(time.Hour),
	})
	var unav *telemetry.UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("want an UnavailableError, got %v", err)
	}
	if !strings.Contains(unav.Reason, "svc-callback") {
		t.Errorf("reason = %q, want it to quote the prefix that matched nothing", unav.Reason)
	}
	if !strings.Contains(unav.Reason, "naming conventions") {
		t.Errorf("reason = %q, want it to say the handle was a guess", unav.Reason)
	}
	if res.Series == nil || res.Queried == nil || res.Warnings == nil {
		t.Error("every slice must be a real array even on the refusal path")
	}
}

// TestMetricsNoHandles — a service with no metric handle at all gets a
// different reason from one whose handle resolved to nothing.
func TestMetricsNoHandles(t *testing.T) {
	p := newMetricProvider(t, &fakeCWM{})
	_, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{{
			Kind: registry.KindCloudWatchLogs, Ref: "some-group",
		}},
		Start: base, End: base.Add(time.Hour),
	})
	var unav *telemetry.UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("want an UnavailableError, got %v", err)
	}
	if !strings.Contains(unav.Reason, "no CloudWatch metric handles") {
		t.Errorf("reason = %q", unav.Reason)
	}
}

// TestPeriodRaisedForOldWindow — CloudWatch answers a too-fine period
// over an old window with an EMPTY result and no error. The period must
// be raised, and the raise must be reported, or the panel draws a blank
// chart labelled with a resolution the data never had.
func TestPeriodRaisedForOldWindow(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		ageDays    int
		requested  time.Duration
		wantPeriod time.Duration
		wantAdjust bool
	}{
		{"fresh window keeps 1m", 1, time.Minute, time.Minute, false},
		{"20 days old is raised to 5m", 20, time.Minute, 5 * time.Minute, true},
		{"90 days old is raised to 1h", 90, time.Minute, time.Hour, true},
		{"a coarser request is left alone", 20, time.Hour, time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := now.AddDate(0, 0, -tc.ageDays)
			end := start.Add(time.Hour)
			p, reason, err := choosePeriod(start, end, now, tc.requested)
			if err != nil {
				t.Fatalf("choosePeriod: %v", err)
			}
			if p != tc.wantPeriod {
				t.Errorf("period = %s, want %s", p, tc.wantPeriod)
			}
			if (reason != "") != tc.wantAdjust {
				t.Errorf("adjust reason = %q, wantAdjust=%v", reason, tc.wantAdjust)
			}
			if tc.wantAdjust && !strings.Contains(reason, "no points at all") {
				t.Errorf("reason = %q, want it to explain that a finer period returns silence", reason)
			}
		})
	}
}

// TestPeriodBeyondRetentionRefuses — past 455 days there is nothing to
// coarsen to, so the answer is a reason rather than an empty chart.
func TestPeriodBeyondRetentionRefuses(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	start := now.AddDate(-2, 0, 0)
	_, _, err := choosePeriod(start, start.Add(time.Hour), now, 0)
	var unav *telemetry.UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("want an UnavailableError, got %v", err)
	}
	if !strings.Contains(unav.Reason, "455") {
		t.Errorf("reason = %q, want it to name the retention limit", unav.Reason)
	}
}

// TestAutoPeriodBoundsPointCount — the auto-period must keep a wide
// window inside the per-series point ceiling, because the response goes
// through a Function URL with a hard size cap.
func TestAutoPeriodBoundsPointCount(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, window := range []time.Duration{
		time.Hour, 3 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour,
	} {
		start := now.Add(-window)
		p, _, err := choosePeriod(start, now, now, 0)
		if err != nil {
			t.Fatalf("choosePeriod(%s): %v", window, err)
		}
		if p < time.Minute {
			t.Errorf("window %s: period %s is below CloudWatch's finest standard resolution", window, p)
		}
		if p > time.Minute && p%time.Minute != 0 {
			t.Errorf("window %s: period %s is not a multiple of 60s, which CloudWatch refuses", window, p)
		}
		if n := int(window / p); n > maxPoints {
			t.Errorf("window %s at %s yields %d points, over the %d ceiling", window, p, n, maxPoints)
		}
	}
}

// TestRequestedPeriodReachesCloudWatch — the period and statistic in
// the response must be the ones that were actually sent.
func TestRequestedPeriodReachesCloudWatch(t *testing.T) {
	f := &fakeCWM{
		listed: map[string][]cwmtypes.Metric{"AWS/Lambda": {
			lambdaMetric("svc-callback-1", "Duration"),
		}},
		values: map[string][]float64{"Duration|svc-callback-1": {1, 2}},
	}
	p := newMetricProvider(t, f)
	res, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{lambdaSource("svc")},
		Names:   []string{"Duration"},
		Period:  5 * time.Minute,
		Start:   base, End: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if res.PeriodSeconds != 300 {
		t.Errorf("PeriodSeconds = %d, want 300", res.PeriodSeconds)
	}
	if got := aws.ToInt32(f.lastGet[0].MetricStat.Period); got != 300 {
		t.Errorf("period sent to CloudWatch = %d, want 300", got)
	}
	if got := aws.ToString(f.lastGet[0].MetricStat.Stat); got != "Average" {
		t.Errorf("stat sent = %q, want Average for Duration", got)
	}
	// The id must satisfy CloudWatch's ^[a-z][a-zA-Z0-9_]*$ — a metric
	// name does not.
	if id := aws.ToString(f.lastGet[0].Id); id != "m0" {
		t.Errorf("query id = %q, want a generated identifier", id)
	}
}

// TestMetricsFanOutCapIsReported — a chart silently missing one of
// three functions is the dashboard reporting a partial sum as a total.
func TestMetricsFanOutCapIsReported(t *testing.T) {
	var listed []cwmtypes.Metric
	for _, fn := range []string{"svc-callback-1", "svc-callback-2", "svc-callback-3"} {
		for _, m := range []string{"Invocations", "Errors"} {
			listed = append(listed, lambdaMetric(fn, m))
		}
	}
	f := &fakeCWM{listed: map[string][]cwmtypes.Metric{"AWS/Lambda": listed}}
	p := newMetricProvider(t, f)

	res, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources:   []registry.TelemetrySource{lambdaSource("svc")},
		Names:     []string{"Invocations", "Errors"},
		MaxSeries: 3,
		Start:     base, End: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if !res.Capped {
		t.Fatal("six series over a cap of three must report Capped")
	}
	if !strings.Contains(res.CapReason, "3 of 6") {
		t.Errorf("CapReason = %q, want it to count what was left out", res.CapReason)
	}
	if len(res.Series) != 3 {
		t.Errorf("got %d series, want the cap honoured", len(res.Series))
	}
	// Several functions ⇒ the legend must distinguish them.
	for _, s := range res.Series {
		if !strings.Contains(s.Label, "svc-callback-") {
			t.Errorf("label %q does not name its target", s.Label)
		}
	}
}

// TestMetricsListFailureIsAWarningNotSilence — a discovery failure must
// not present as a service with no metrics.
func TestMetricsListFailureIsAWarningNotSilence(t *testing.T) {
	f := &fakeCWM{listErr: errors.New("AccessDeniedException: cloudwatch:ListMetrics")}
	p := newMetricProvider(t, f)
	res, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{lambdaSource("svc")},
		Start:   base, End: base.Add(time.Hour),
	})
	var unav *telemetry.UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("want an UnavailableError carrying the AWS text, got %v", err)
	}
	if !strings.Contains(unav.Reason, "ListMetrics") {
		t.Errorf("reason = %q, want the AWS error verbatim so the missing grant is actionable", unav.Reason)
	}
	if len(res.Warnings) == 0 {
		t.Error("the failure must also survive as a warning on the result")
	}
}

// TestMetricsReadFailureIsAnError — every region failing is a fault,
// not a quiet workload, and belongs in a red box.
func TestMetricsReadFailureIsAnError(t *testing.T) {
	f := &fakeCWM{
		listed: map[string][]cwmtypes.Metric{"AWS/Lambda": {
			lambdaMetric("svc-callback-1", "Invocations"),
		}},
		getErr: errors.New("AccessDeniedException: cloudwatch:GetMetricData"),
	}
	p := newMetricProvider(t, f)
	_, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{lambdaSource("svc")},
		Names:   []string{"Invocations"},
		Start:   base, End: base.Add(time.Hour),
	})
	if err == nil {
		t.Fatal("want an error when the only region's read fails")
	}
	var unav *telemetry.UnavailableError
	if errors.As(err, &unav) {
		t.Error("a denied read is a fault, not an UnavailableError")
	}
}

// TestMetricsExactDimensionHandle — the ECS shape, where the handle
// records a concrete dimension and no resolution is needed.
func TestMetricsExactDimensionHandle(t *testing.T) {
	f := &fakeCWM{
		listed: map[string][]cwmtypes.Metric{"AWS/ECS": {{
			Namespace:  aws.String("AWS/ECS"),
			MetricName: aws.String("CPUUtilization"),
			Dimensions: []cwmtypes.Dimension{{Name: aws.String("ClusterName"), Value: aws.String("svc")}},
		}}},
		values: map[string][]float64{"CPUUtilization|svc": {41.5}},
	}
	p := newMetricProvider(t, f)
	res, err := p.Metrics(context.Background(), telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{{
			Kind: registry.KindCloudWatchMetrics, Ref: "AWS/ECS", Region: "eu-central-1",
			Dims: map[string]string{"ClusterName": "svc"},
		}},
		Start: base, End: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	cpu, ok := seriesByName(res, "CPUUtilization")
	if !ok {
		t.Fatal("CPUUtilization missing")
	}
	if len(cpu.Points) != 1 || cpu.Points[0].Value != 41.5 {
		t.Errorf("points = %v", cpu.Points)
	}
	if cpu.Unit != "Percent" {
		t.Errorf("unit = %q, want Percent so the axis can be labelled", cpu.Unit)
	}
	// MemoryUtilization is in the catalogue but not in the inventory.
	mem, _ := seriesByName(res, "MemoryUtilization")
	if !mem.Absent {
		t.Error("MemoryUtilization must be reported absent, not omitted")
	}
}

// TestListMetricsCached — discovery is cached per handle, because a
// 30-second poll would otherwise spend a ListMetrics call per panel
// refresh on an inventory that only changes on deploy.
func TestListMetricsCached(t *testing.T) {
	f := &fakeCWM{
		listed: map[string][]cwmtypes.Metric{"AWS/Lambda": {
			lambdaMetric("svc-callback-1", "Invocations"),
		}},
	}
	p := newMetricProvider(t, f)
	q := telemetry.MetricQuery{
		Sources: []registry.TelemetrySource{lambdaSource("svc")},
		Names:   []string{"Invocations"},
		Start:   base, End: base.Add(time.Hour),
	}
	for i := 0; i < 3; i++ {
		if _, err := p.Metrics(context.Background(), q); err != nil {
			t.Fatalf("Metrics: %v", err)
		}
	}
	if f.listCalls != 1 {
		t.Errorf("ListMetrics called %d times, want 1 (cached within the TTL)", f.listCalls)
	}
	if f.getCalls != 3 {
		t.Errorf("GetMetricData called %d times, want one per query — the DATA is never cached", f.getCalls)
	}
}
