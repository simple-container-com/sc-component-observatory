// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package awsprov

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
)

var base = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// fakeCW behaves like CloudWatch Logs for the parts that matter: it
// honours the window, pages with a token, and can fail per group.
//
// Modelled rather than stubbed because the three things most likely to
// be subtly wrong here — the window boundary a cursor produces, the
// ring buffer that keeps the newest page, and the budget arithmetic —
// are all invisible to a live smoke, which only ever shows whether
// SOME logs came back.
type fakeCW struct {
	groups    []string
	events    map[string][]cwtypes.FilteredLogEvent // group → ascending by ts
	failGroup map[string]error
	pageSize  int // events per FilterLogEvents response; 0 ⇒ all

	filterCalls   int
	describeCalls int
	describeErr   error
	lastFilter    cloudwatchlogs.FilterLogEventsInput

	// filters records EVERY FilterLogEvents input, because the scan
	// reads a window as several sub-windows now — asserting on the last
	// one alone would have said nothing about the newest, which is the
	// one a tail depends on.
	filters []cloudwatchlogs.FilterLogEventsInput
}

func (f *fakeCW) DescribeLogGroups(_ context.Context, in *cloudwatchlogs.DescribeLogGroupsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error) {
	f.describeCalls++
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	prefix := aws.ToString(in.LogGroupNamePrefix)
	out := &cloudwatchlogs.DescribeLogGroupsOutput{}
	for _, g := range f.groups {
		if strings.HasPrefix(g, prefix) {
			out.LogGroups = append(out.LogGroups, cwtypes.LogGroup{LogGroupName: aws.String(g)})
		}
	}
	return out, nil
}

func (f *fakeCW) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	f.filterCalls++
	f.lastFilter = *in
	f.filters = append(f.filters, *in)
	group := aws.ToString(in.LogGroupName)
	if err, ok := f.failGroup[group]; ok {
		return nil, err
	}
	var inWindow []cwtypes.FilteredLogEvent
	for _, e := range f.events[group] {
		ts := aws.ToInt64(e.Timestamp)
		if in.StartTime != nil && ts < *in.StartTime {
			continue
		}
		// EndTime is exclusive, as it is in the real API.
		if in.EndTime != nil && ts >= *in.EndTime {
			continue
		}
		inWindow = append(inWindow, e)
	}
	from := 0
	if in.NextToken != nil {
		if _, err := fmt.Sscanf(*in.NextToken, "%d", &from); err != nil {
			return nil, fmt.Errorf("bad token %q", *in.NextToken)
		}
	}
	size := f.pageSize
	if size <= 0 || size > len(inWindow)-from {
		size = len(inWindow) - from
	}
	out := &cloudwatchlogs.FilterLogEventsOutput{Events: inWindow[from : from+size]}
	if from+size < len(inWindow) {
		out.NextToken = aws.String(fmt.Sprintf("%d", from+size))
	}
	return out, nil
}

func ev(id string, offset time.Duration, msg string) cwtypes.FilteredLogEvent {
	return cwtypes.FilteredLogEvent{
		EventId:       aws.String(id),
		Timestamp:     aws.Int64(base.Add(offset).UnixMilli()),
		Message:       aws.String(msg + "\n"),
		LogStreamName: aws.String("2026/11/22/[$LATEST]abc"),
	}
}

func newProvider(t *testing.T, f *fakeCW) *Provider {
	t.Helper()
	p, err := New(context.Background(), Config{
		DefaultRegion: "eu-central-1",
		Now:           func() time.Time { return base.Add(time.Hour) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.clientFor = func(context.Context, string) (cwAPI, error) { return f, nil }
	return p
}

func prefixSource(ref string) registry.TelemetrySource {
	return registry.TelemetrySource{
		Kind: SourceKindLogs, Ref: ref, Region: "eu-central-1",
		Dims: map[string]string{DimMatch: DimMatchPrefix},
	}
}

func exactSource(ref string) registry.TelemetrySource {
	return registry.TelemetrySource{Kind: SourceKindLogs, Ref: ref, Region: "eu-central-1"}
}

func window(q *telemetry.LogQuery) {
	q.Start = base.Add(-time.Hour)
	q.End = base.Add(time.Hour)
}

// TestLogs_BusyWindowTailIsTheTrueNewest is the regression test for the
// 2026-11-22 live finding, and the reason the scan reads the window
// backwards in chunks.
//
// A loud log group with a modest budget is the case that was wrong: a
// forward walk spent its whole allowance on the window's oldest minutes
// and returned those events as "the latest logs", under a header saying
// the window ended now. On staging that meant an hour-stale tail for
// every unfiltered query against forge-conductor.
//
// The budget here is deliberately far too small to read the window:
// 1000 events at a page size of 100 would need ten calls and four are
// allowed. Reading from the newest end makes that irrelevant — and the
// page must come back NOT capped, because nothing was missed that could
// have belonged on it.
func TestLogs_BusyWindowTailIsTheTrueNewest(t *testing.T) {
	var evs []cwtypes.FilteredLogEvent
	for i := 0; i < 1000; i++ {
		// 3.6s apart, filling the hour before `base`.
		evs = append(evs, ev(fmt.Sprintf("e%d", i), -time.Duration(1000-i)*3600*time.Millisecond, fmt.Sprintf("line-%d", i)))
	}
	f := &fakeCW{
		groups:   []string{"/aws/lambda/svc"},
		events:   map[string][]cwtypes.FilteredLogEvent{"/aws/lambda/svc": evs},
		pageSize: 100,
	}
	p := newProvider(t, f)
	page, err := p.Logs(context.Background(), telemetry.LogQuery{
		Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")},
		Limit:   5,
		Start:   base.Add(-time.Hour),
		End:     base,
		Budget:  telemetry.Budget{MaxCalls: 4},
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(page.Events) != 5 {
		t.Fatalf("want a full page of 5, got %d", len(page.Events))
	}
	// e999 is the newest event in the window. Before the fix this was
	// e0..e4 — the oldest.
	if page.Events[0].Message != "line-999" {
		t.Errorf("newest event = %q, want line-999: the tail is showing the wrong end of the window",
			page.Events[0].Message)
	}
	if page.Events[4].Message != "line-995" {
		t.Errorf("oldest event on the page = %q, want line-995", page.Events[4].Message)
	}
	if page.Capped {
		t.Errorf("page reports capped=%v (%q), but it holds the window's true newest events — a cap here would send the reader looking for data that is already on screen",
			page.Capped, page.CapReason)
	}
	// It must also be CHEAP: finding the newest five events should not
	// read the hour. One 30-second chunk holds more than five.
	if page.Scanned > 50 {
		t.Errorf("scanned %d events to return 5 — the backward walk is not stopping early", page.Scanned)
	}
	if page.Calls > 2 {
		t.Errorf("spent %d calls on a tail, want 1-2", page.Calls)
	}
	if page.NextCursor == "" {
		t.Error("a full page must carry a cursor; there is an hour of older events behind it")
	}
}

// TestLogs_ResponseByteCapTrimsThePageNotTheScan. MaxBytes exists for
// the Function URL's hard response limit, so it must bound what is
// RETURNED. Counting it against the scan instead is what stopped the
// walk five minutes into a sixty-minute window — and it did that while
// returning a three-event page of a few KiB, nowhere near the cap it
// claimed to have hit.
func TestLogs_ResponseByteCapTrimsThePageNotTheScan(t *testing.T) {
	big := strings.Repeat("x", 1000)
	var evs []cwtypes.FilteredLogEvent
	for i := 0; i < 5; i++ {
		evs = append(evs, ev(fmt.Sprintf("e%d", i), time.Duration(i)*time.Second, fmt.Sprintf("%d-%s", i, big)))
	}
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc"},
		events: map[string][]cwtypes.FilteredLogEvent{"/aws/lambda/svc": evs},
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{
		Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")},
		Limit:   10,
		Budget:  telemetry.Budget{MaxBytes: 2500},
	}
	window(&q)
	page, err := p.Logs(context.Background(), q)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("want the newest 2 events to fit 2500 bytes, got %d", len(page.Events))
	}
	// Newest kept, oldest dropped — the same direction "load older" goes.
	if !strings.HasPrefix(page.Events[0].Message, "4-") {
		t.Errorf("newest returned event = %.10q, want event 4: the trim took the wrong end", page.Events[0].Message)
	}
	if !page.Capped || !strings.Contains(page.CapReason, "response-size cap") {
		t.Errorf("a trimmed page must say so: capped=%v reason=%q", page.Capped, page.CapReason)
	}
	// Short page, but there IS more — so the cursor has to be there or
	// the dropped events are unreachable.
	if page.NextCursor == "" {
		t.Error("no cursor on a byte-trimmed page: the events it dropped would be unreachable")
	}
	// And the scan itself must not have been cut short by the byte cap.
	if page.Scanned != 5 {
		t.Errorf("scanned = %d, want all 5: MaxBytes must not bound the scan", page.Scanned)
	}
}

// TestLogs_ReturnsNewestFirstAndTails is the core promise: the page is
// the NEWEST events in the window, not the first ones CloudWatch hands
// back. FilterLogEvents only walks forward, so getting this wrong is
// the easy mistake — and it would present the oldest end of a window as
// "latest logs".
func TestLogs_ReturnsNewestFirstAndTails(t *testing.T) {
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc-callback-abc"},
		events: map[string][]cwtypes.FilteredLogEvent{
			"/aws/lambda/svc-callback-abc": {
				ev("1", 0, "first"), ev("2", time.Minute, "second"),
				ev("3", 2*time.Minute, "third"), ev("4", 3*time.Minute, "fourth"),
				ev("5", 4*time.Minute, "fifth"),
			},
		},
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{Sources: []registry.TelemetrySource{prefixSource("/aws/lambda/svc-callback")}, Limit: 2}
	window(&q)

	page, err := p.Logs(context.Background(), q)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("want 2 events, got %d", len(page.Events))
	}
	if page.Events[0].Message != "fifth" || page.Events[1].Message != "fourth" {
		t.Fatalf("want newest-first [fifth fourth], got %q %q", page.Events[0].Message, page.Events[1].Message)
	}
	if page.Events[0].Truncated {
		t.Error("short message marked truncated")
	}
	// The trailing newline the runtime wrote must not reach the UI.
	if strings.HasSuffix(page.Events[0].Message, "\n") {
		t.Error("trailing newline not trimmed")
	}
	if page.Scanned != 5 {
		t.Errorf("scanned = %d, want 5", page.Scanned)
	}
	if page.Capped {
		t.Errorf("unexpectedly capped: %s", page.CapReason)
	}
	if len(page.Queried) != 1 || page.Queried[0] != "/aws/lambda/svc-callback-abc" {
		t.Errorf("queried = %v, want the resolved concrete group", page.Queried)
	}
	if page.NextCursor == "" {
		t.Error("a full page must offer a cursor")
	}
}

// TestLogs_CursorWalksBackwardsWithoutGapOrRepeat exercises the
// boundary rule the cursor exists for: the next page must start exactly
// where this one ended, with no event shown twice and none skipped.
func TestLogs_CursorWalksBackwardsWithoutGapOrRepeat(t *testing.T) {
	var evs []cwtypes.FilteredLogEvent
	for i := 0; i < 6; i++ {
		evs = append(evs, ev(fmt.Sprintf("e%d", i), time.Duration(i)*time.Minute, fmt.Sprintf("line-%d", i)))
	}
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc"},
		events: map[string][]cwtypes.FilteredLogEvent{"/aws/lambda/svc": evs},
	}
	p := newProvider(t, f)

	seen := map[string]int{}
	cursor := ""
	for pageNum := 0; pageNum < 3; pageNum++ {
		q := telemetry.LogQuery{Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")}, Limit: 2, Cursor: cursor}
		window(&q)
		page, err := p.Logs(context.Background(), q)
		if err != nil {
			t.Fatalf("page %d: %v", pageNum, err)
		}
		if len(page.Events) != 2 {
			t.Fatalf("page %d: want 2 events, got %d", pageNum, len(page.Events))
		}
		for _, e := range page.Events {
			seen[e.Message]++
		}
		cursor = page.NextCursor
	}
	if len(seen) != 6 {
		t.Fatalf("want all 6 distinct lines across 3 pages, got %d: %v", len(seen), seen)
	}
	for msg, n := range seen {
		if n != 1 {
			t.Errorf("%s returned %d times — the cursor repeated an event", msg, n)
		}
	}
}

// TestLogs_CursorDoesNotDropEventsSharingTheBoundaryMillisecond is the
// reason the cursor carries ids at all. A page that cuts through a
// burst of same-millisecond lines must resume inside that millisecond.
func TestLogs_CursorDoesNotDropEventsSharingTheBoundaryMillisecond(t *testing.T) {
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc"},
		events: map[string][]cwtypes.FilteredLogEvent{"/aws/lambda/svc": {
			ev("a", 0, "burst-a"), ev("b", 0, "burst-b"), ev("c", 0, "burst-c"),
			ev("d", time.Minute, "later"),
		}},
	}
	p := newProvider(t, f)

	q := telemetry.LogQuery{Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")}, Limit: 2}
	window(&q)
	first, err := p.Logs(context.Background(), q)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	// "later" plus ONE of the burst.
	if len(first.Events) != 2 {
		t.Fatalf("want 2, got %d", len(first.Events))
	}

	q2 := telemetry.LogQuery{Sources: q.Sources, Limit: 2, Cursor: first.NextCursor}
	window(&q2)
	second, err := p.Logs(context.Background(), q2)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	got := map[string]bool{}
	for _, e := range append(append([]telemetry.LogEvent{}, first.Events...), second.Events...) {
		if got[e.ID] {
			t.Errorf("event %s returned twice", e.ID)
		}
		got[e.ID] = true
	}
	for _, want := range []string{"a", "b", "c", "d"} {
		if !got[want] {
			t.Errorf("event %s was dropped at the page boundary", want)
		}
	}
}

// TestLogs_ScanCapIsReportedNotHidden — a capped scan must say so, and
// say what the result then means. A capped page that looks like a
// complete one is the silent degradation this component exists to stop.
func TestLogs_ScanCapIsReportedNotHidden(t *testing.T) {
	var evs []cwtypes.FilteredLogEvent
	for i := 0; i < 50; i++ {
		evs = append(evs, ev(fmt.Sprintf("e%d", i), time.Duration(i)*time.Second, "line"))
	}
	f := &fakeCW{
		groups:   []string{"/aws/lambda/svc"},
		events:   map[string][]cwtypes.FilteredLogEvent{"/aws/lambda/svc": evs},
		pageSize: 5,
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{
		Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")},
		Limit:   3,
		Budget:  telemetry.Budget{MaxCalls: 2},
	}
	window(&q)
	page, err := p.Logs(context.Background(), q)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !page.Capped {
		t.Fatal("want Capped=true after exhausting the call budget")
	}
	if page.CapReason == "" {
		t.Fatal("Capped with no reason is unactionable")
	}
	// The reason must warn that this is NOT the newest of the window —
	// the whole point of surfacing it — and name the limit that bound,
	// because "ran out" alone does not tell a reader which knob moved.
	if !strings.Contains(page.CapReason, "NOT of the whole window") {
		t.Errorf("cap reason does not say the result is a partial scan: %q", page.CapReason)
	}
	if !strings.Contains(page.CapReason, "call cap") {
		t.Errorf("cap reason does not name the CALL cap as what stopped it: %q", page.CapReason)
	}
	// Since the scan reads newest-chunk-first, a capped page must also
	// say which interval it read — the timestamps alone cannot tell a
	// reader that the rest of the window was never looked at.
	if !strings.Contains(page.CapReason, "OLDEST events of that interval") {
		t.Errorf("cap reason does not say which end of which interval this is: %q", page.CapReason)
	}
	if page.Calls != 2 {
		t.Errorf("calls = %d, want exactly the budget (2)", page.Calls)
	}
}

// TestLogs_EventScanCap bounds the CloudWatch bill independently of the
// call count — a single wide call can scan a lot.
func TestLogs_EventScanCap(t *testing.T) {
	var evs []cwtypes.FilteredLogEvent
	for i := 0; i < 40; i++ {
		evs = append(evs, ev(fmt.Sprintf("e%d", i), time.Duration(i)*time.Second, "line"))
	}
	f := &fakeCW{
		groups:   []string{"/aws/lambda/svc"},
		events:   map[string][]cwtypes.FilteredLogEvent{"/aws/lambda/svc": evs},
		pageSize: 10,
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{
		Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")},
		Limit:   5,
		Budget:  telemetry.Budget{MaxEventsScanned: 15},
	}
	window(&q)
	page, err := p.Logs(context.Background(), q)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !page.Capped || !strings.Contains(page.CapReason, "the scan cap stopped") {
		t.Fatalf("want a scan-cap stop, got capped=%v reason=%q", page.Capped, page.CapReason)
	}
	if page.Scanned > 20 {
		t.Errorf("scanned %d events despite a cap of 15 — the cap is not holding", page.Scanned)
	}
}

// TestLogs_UnresolvedPrefixSaysTheGuessMayBeWrong. An inferred handle
// that matches no live log group produces the same empty list as a
// quiet service. It must not.
func TestLogs_UnresolvedPrefixSaysTheGuessMayBeWrong(t *testing.T) {
	f := &fakeCW{groups: []string{"/aws/lambda/some-other-service"}}
	p := newProvider(t, f)
	q := telemetry.LogQuery{Sources: []registry.TelemetrySource{prefixSource("/aws/lambda/svc-callback")}, Limit: 10}
	window(&q)

	_, err := p.Logs(context.Background(), q)
	var unav *telemetry.UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("want an UnavailableError, got %v", err)
	}
	if !strings.Contains(unav.Reason, "inferred") {
		t.Errorf("reason does not flag the inference: %q", unav.Reason)
	}
	if !strings.Contains(unav.Reason, "/aws/lambda/svc-callback") {
		t.Errorf("reason does not name the prefix it tried: %q", unav.Reason)
	}
}

// TestLogs_NoHandlesOfOurKind — a service with only metric handles has
// nothing to read, and that is a reason rather than a failure.
func TestLogs_NoHandlesOfOurKind(t *testing.T) {
	p := newProvider(t, &fakeCW{})
	q := telemetry.LogQuery{
		Sources: []registry.TelemetrySource{{Kind: "cloudwatch-metrics", Ref: "AWS/Lambda"}},
		Limit:   10,
	}
	window(&q)
	_, err := p.Logs(context.Background(), q)
	var unav *telemetry.UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("want UnavailableError, got %v", err)
	}
}

// TestLogs_PrefixResolutionIsCached — one DescribeLogGroups per prefix
// per TTL, not one per page. A cold Lambda pays it once.
func TestLogs_PrefixResolutionIsCached(t *testing.T) {
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc-callback-abc"},
		events: map[string][]cwtypes.FilteredLogEvent{
			"/aws/lambda/svc-callback-abc": {ev("1", 0, "x")},
		},
	}
	p := newProvider(t, f)
	for i := 0; i < 3; i++ {
		q := telemetry.LogQuery{Sources: []registry.TelemetrySource{prefixSource("/aws/lambda/svc-callback")}, Limit: 10}
		window(&q)
		if _, err := p.Logs(context.Background(), q); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
	}
	if f.describeCalls != 1 {
		t.Errorf("DescribeLogGroups called %d times, want 1 (cached)", f.describeCalls)
	}
}

// TestLogs_PartialFailureKeepsTheWorkingGroup. One denied log group out
// of two must not sink the other, and must not vanish either.
func TestLogs_PartialFailureKeepsTheWorkingGroup(t *testing.T) {
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc-a", "/aws/lambda/svc-b"},
		events: map[string][]cwtypes.FilteredLogEvent{
			"/aws/lambda/svc-b": {ev("1", 0, "from-b")},
		},
		failGroup: map[string]error{
			"/aws/lambda/svc-a": errors.New("operation error CloudWatch Logs: FilterLogEvents, https response error StatusCode: 400, RequestID: x, AccessDeniedException"),
		},
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{Sources: []registry.TelemetrySource{prefixSource("/aws/lambda/svc-")}, Limit: 10}
	window(&q)

	page, err := p.Logs(context.Background(), q)
	if err != nil {
		t.Fatalf("one bad group must not sink the query: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].Message != "from-b" {
		t.Fatalf("want the working group's event, got %+v", page.Events)
	}
	if len(page.Warnings) != 1 || !strings.Contains(page.Warnings[0], "AccessDeniedException") {
		t.Fatalf("want a warning naming the denial, got %v", page.Warnings)
	}
	if !strings.Contains(page.Warnings[0], "/aws/lambda/svc-a") {
		t.Errorf("warning does not name the failing group: %q", page.Warnings[0])
	}
}

// TestLogs_TotalFailureIsAnError — when nothing could be read, an empty
// page with a warning would be read as "no logs". It is an error.
func TestLogs_TotalFailureIsAnError(t *testing.T) {
	f := &fakeCW{
		groups:    []string{"/aws/lambda/svc"},
		failGroup: map[string]error{"/aws/lambda/svc": errors.New("AccessDeniedException")},
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")}, Limit: 10}
	window(&q)
	if _, err := p.Logs(context.Background(), q); err == nil {
		t.Fatal("want an error when every group failed")
	}
}

// TestLogs_MissingGroupIsUnavailableNotAFailure. An EXACT handle has no
// resolution step, so a group that was never created arrives as a
// ResourceNotFoundException from the read itself. That is the derivation
// naming a group nobody made — a reason — and reporting it as a failed
// query would read as a broken dashboard. Live-caught 2026-11-22: every
// tenant's Lambda logs live in `<stack>-access-logs`, not under
// `/aws/lambda/`, and the account holds no `/aws/lambda/*` group at all.
func TestLogs_MissingGroupIsUnavailableNotAFailure(t *testing.T) {
	f := &fakeCW{
		groups: []string{"svc--staging-access-logs"},
		failGroup: map[string]error{
			"svc--staging-access-logs": &cwtypes.ResourceNotFoundException{
				Message: aws.String("The specified log group does not exist."),
			},
		},
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{
		Sources: []registry.TelemetrySource{exactSource("svc--staging-access-logs")},
		Limit:   10,
	}
	window(&q)
	page, err := p.Logs(context.Background(), q)

	var unav *telemetry.UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("err = %v (%T), want an UnavailableError", err, err)
	}
	if !strings.Contains(unav.Reason, "svc--staging-access-logs") {
		t.Errorf("reason %q does not name the group that is missing", unav.Reason)
	}
	// The group it tried still rides along: "which group did you look
	// in" is the first question, and the error path is where it is
	// asked most.
	if len(page.Queried) != 1 || page.Queried[0] != "svc--staging-access-logs" {
		t.Errorf("Queried = %v, want the group carried through the error path", page.Queried)
	}
}

// TestLogs_MissingAndDeniedTogetherIsStillAFailure — softening a mixed
// result to "unavailable" would hide the grant that needs fixing.
func TestLogs_MissingAndDeniedTogetherIsStillAFailure(t *testing.T) {
	f := &fakeCW{
		groups: []string{"a-access-logs", "b-access-logs"},
		failGroup: map[string]error{
			"a-access-logs": &cwtypes.ResourceNotFoundException{Message: aws.String("gone")},
			"b-access-logs": errors.New("AccessDeniedException: logs:FilterLogEvents"),
		},
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{Sources: []registry.TelemetrySource{
		exactSource("a-access-logs"), exactSource("b-access-logs"),
	}, Limit: 10}
	window(&q)
	_, err := p.Logs(context.Background(), q)

	var unav *telemetry.UnavailableError
	if err == nil || errors.As(err, &unav) {
		t.Fatalf("err = %v, want a hard failure when one of the groups was DENIED", err)
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err = %q, want the denial text so the missing grant is actionable", err)
	}
}

// TestLogs_LongMessageTruncatedAndFlagged — one huge line must not eat
// the page's byte budget, and the cut must be visible.
func TestLogs_LongMessageTruncatedAndFlagged(t *testing.T) {
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc"},
		events: map[string][]cwtypes.FilteredLogEvent{
			"/aws/lambda/svc": {ev("1", 0, strings.Repeat("x", 5000))},
		},
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{
		Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")},
		Limit:   10,
		Budget:  telemetry.Budget{MaxMessageBytes: 100},
	}
	window(&q)
	page, err := p.Logs(context.Background(), q)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("want 1 event, got %d", len(page.Events))
	}
	if len(page.Events[0].Message) != 100 {
		t.Errorf("message length = %d, want 100", len(page.Events[0].Message))
	}
	if !page.Events[0].Truncated {
		t.Error("truncated message not flagged")
	}
}

// TestLogs_SubWindowsTileTheWindowExactly — the provider must read the
// window it was given, no more and no less. It no longer reads it in
// one call, so "verbatim" now means the sub-windows TILE it: the first
// one ends where the window ends (or a tail is stale), the last begins
// where it begins (or the oldest events are unreachable), and none of
// them reaches outside (or the page shows a period the UI is not
// displaying).
func TestLogs_SubWindowsTileTheWindowExactly(t *testing.T) {
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc"},
		events: map[string][]cwtypes.FilteredLogEvent{"/aws/lambda/svc": {ev("1", 0, "x")}},
	}
	p := newProvider(t, f)
	start, end := base.Add(-5*time.Minute), base.Add(5*time.Minute)
	_, err := p.Logs(context.Background(), telemetry.LogQuery{
		Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")},
		Limit:   10, Start: start, End: end, Filter: "ERROR",
	})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if len(f.filters) == 0 {
		t.Fatal("no FilterLogEvents call was made")
	}
	if got := aws.ToInt64(f.filters[0].EndTime); got != end.UnixMilli() {
		t.Errorf("first sub-window ends at %d, want the window end %d — a tail that starts earlier is already stale",
			got, end.UnixMilli())
	}
	oldest := aws.ToInt64(f.filters[len(f.filters)-1].StartTime)
	if oldest != start.UnixMilli() {
		t.Errorf("last sub-window starts at %d, want the window start %d — the rest of the window is unreachable",
			oldest, start.UnixMilli())
	}
	// Deduplicated: paginating one sub-window repeats its bounds with a
	// new token, which is not a second sub-window.
	var bounds [][2]int64
	for i, in := range f.filters {
		s, e := aws.ToInt64(in.StartTime), aws.ToInt64(in.EndTime)
		if s < start.UnixMilli() || e > end.UnixMilli() {
			t.Errorf("sub-window %d (%d–%d) reaches outside the requested window", i, s, e)
		}
		if got := aws.ToString(in.FilterPattern); got != "ERROR" {
			t.Errorf("sub-window %d FilterPattern = %q, want it passed through verbatim", i, got)
		}
		if n := len(bounds); n == 0 || bounds[n-1] != [2]int64{s, e} {
			bounds = append(bounds, [2]int64{s, e})
		}
	}
	// Contiguous and strictly older, so nothing between two chunks is
	// skipped and nothing is read twice.
	for i := 1; i < len(bounds); i++ {
		if bounds[i][1] != bounds[i-1][0] {
			t.Errorf("sub-window %d ends at %d but the previous began at %d — a gap or an overlap",
				i, bounds[i][1], bounds[i-1][0])
		}
	}
}

// TestLogs_RejectsAnEmptyWindow — the HTTP layer owns the defaults, so
// a zero window reaching the provider is a bug, not something to guess
// around.
func TestLogs_RejectsAnEmptyWindow(t *testing.T) {
	p := newProvider(t, &fakeCW{})
	_, err := p.Logs(context.Background(), telemetry.LogQuery{
		Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")},
	})
	var unav *telemetry.UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("want UnavailableError for a zero window, got %v", err)
	}
}

// TestLogs_ShortPageOffersNoCursor — a cursor on an exhausted window
// invites an endless "load more" that always returns nothing.
func TestLogs_ShortPageOffersNoCursor(t *testing.T) {
	f := &fakeCW{
		groups: []string{"/aws/lambda/svc"},
		events: map[string][]cwtypes.FilteredLogEvent{"/aws/lambda/svc": {ev("1", 0, "only")}},
	}
	p := newProvider(t, f)
	q := telemetry.LogQuery{Sources: []registry.TelemetrySource{exactSource("/aws/lambda/svc")}, Limit: 10}
	window(&q)
	page, err := p.Logs(context.Background(), q)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if page.NextCursor != "" {
		t.Error("a short page must not offer a cursor")
	}
}

// TestCapabilitiesAndRefusals — the interface is complete, and the two
// unimplemented reads refuse with a reason instead of returning nothing.
func TestCapabilitiesAndRefusals(t *testing.T) {
	p := newProvider(t, &fakeCW{})
	caps := p.Capabilities()
	if !caps.Logs {
		t.Error("logs capability must be true in slice 2a")
	}
	if caps.Metrics || caps.Workloads {
		t.Error("metrics/workloads must report false until they are implemented")
	}
	if _, err := p.Metrics(context.Background(), telemetry.MetricQuery{}); err == nil {
		t.Error("Metrics must refuse rather than return an empty series set")
	}
	if _, err := p.Workloads(context.Background(), telemetry.WorkloadQuery{}); err == nil {
		t.Error("Workloads must refuse rather than return an empty list")
	}
	if p.Name() != registry.CloudAWS {
		t.Errorf("Name = %q, want %q", p.Name(), registry.CloudAWS)
	}
}

// TestHealthExercisesTheLogsGrant — a health probe that passes without
// the log-read permission reports a working provider that cannot read
// anything.
func TestHealthExercisesTheLogsGrant(t *testing.T) {
	f := &fakeCW{describeErr: errors.New("AccessDeniedException")}
	p := newProvider(t, f)
	if err := p.Health(context.Background()); err == nil {
		t.Fatal("want an error when DescribeLogGroups is denied")
	}
	if f.describeCalls == 0 {
		t.Error("Health must actually call the logs API")
	}
}

// TestRing keeps the newest N in order — the data structure the tail
// depends on.
func TestRing(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		r.push(telemetry.LogEvent{ID: fmt.Sprintf("%d", i)})
	}
	got := r.items()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, want := range []string{"2", "3", "4"} {
		if got[i].ID != want {
			t.Errorf("items[%d] = %s, want %s", i, got[i].ID, want)
		}
	}
}
