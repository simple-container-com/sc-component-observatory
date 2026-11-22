// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package awsprov is the AWS implementation of telemetry.Provider.
//
// Logs come from CloudWatch Logs `FilterLogEvents` — inline, paginated
// and scan-capped (this file). Insights (`StartQuery`) is deliberately
// absent: it bills per GB scanned and its results arrive
// asynchronously, which needs a dispatched worker and a second
// deployable (design decision D2). A dashboard panel that can quietly
// bill is worse than one that cannot answer every question.
//
// Metrics come from `GetMetricData`, with `ListMetrics` first so that
// "this workload has no such metric" is distinguishable from "it
// reported nothing" — see metrics.go.
//
// Workloads is declared and refuses honestly until slice 3.5.
package awsprov

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
)

// SourceKindLogs is the TelemetrySource.Kind this provider serves.
const SourceKindLogs = "cloudwatch-logs"

// DimMatch / DimMatchPrefix mark a handle whose Ref is a log-group
// PREFIX rather than an exact name.
//
// Needed because SC names a Lambda `<stack>-callback-<pulumi-suffix>`
// and the suffix is not derivable outside the deploy that created it.
// Until template plugins publish exact handles (slice 4), the inferred
// handle is a prefix and the provider resolves it with
// DescribeLogGroups at query time.
const (
	DimMatch       = "match"
	DimMatchPrefix = "prefix"
)

// cwAPI is the slice of CloudWatch Logs this provider uses. An
// interface rather than the concrete client so the scan, the budget
// arithmetic and the cursor boundary are testable without AWS — the
// parts most likely to be subtly wrong are exactly the parts a live
// smoke cannot isolate.
type cwAPI interface {
	FilterLogEvents(ctx context.Context, in *cloudwatchlogs.FilterLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
	DescribeLogGroups(ctx context.Context, in *cloudwatchlogs.DescribeLogGroupsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.DescribeLogGroupsOutput, error)
}

// Config is the provider's wiring.
type Config struct {
	// DefaultRegion serves handles that carry no region of their own.
	DefaultRegion string

	// AccessKey/SecretKey are the cross-account escape hatch. Normally
	// empty: observatory reads its own account's logs through its
	// execution role, which the template grants (`cloudExtras.awsRoles`)
	// so that no key is created, stored or rotated.
	AccessKey, SecretKey string

	// GroupCacheTTL bounds how long a resolved prefix is reused. Short
	// by default: a redeploy creates a new log group with a new Pulumi
	// suffix, and a stale resolution would read the PREVIOUS
	// deployment's logs — which is worse than a slow query, because it
	// answers confidently with the wrong service's output.
	GroupCacheTTL time.Duration

	// Now is injected for tests.
	Now func() time.Time
}

// Provider implements telemetry.Provider for AWS.
type Provider struct {
	cfg Config

	// clientFor / metricClientFor are swapped in tests. Keyed by region
	// because log groups and metrics are per-region and a multi-region
	// service is normal.
	//
	// Two clients rather than one: CloudWatch Logs and CloudWatch
	// Metrics are separate AWS services with separate SDK packages and
	// separate IAM actions, and conflating them would make a logs-only
	// grant look like a broken metrics implementation.
	clientFor       func(ctx context.Context, region string) (cwAPI, error)
	metricClientFor func(ctx context.Context, region string) (cwmAPI, error)

	mu       sync.Mutex
	clients  map[string]cwAPI
	mclients map[string]cwmAPI
	groups   map[string]groupCacheEntry
	metrics  map[string]metricCacheEntry
}

type groupCacheEntry struct {
	groups []string
	at     time.Time
}

type metricCacheEntry struct {
	targets []listedTarget
	at      time.Time
}

// Defaults chosen to make capping rare rather than to make it
// impossible — the scan cap exists, and a dashboard that silently
// widens it is the thing the risk register warns about.
const (
	defaultGroupCacheTTL = 2 * time.Minute

	// defaultMaxCalls bounds wall clock. A Lambda that blows its
	// timeout returns no page at all, which reads as an outage rather
	// than as a too-wide query.
	defaultMaxCalls = 16

	// defaultMaxEventsScanned bounds the CloudWatch bill.
	defaultMaxEventsScanned = 20000

	// defaultMaxBytes bounds the RESPONSE — the bytes actually handed
	// back, not the bytes scanned to find them. A Function URL caps its
	// response hard, and exceeding it fails the whole request.
	//
	// The distinction is not pedantry: counting it against the scan is
	// what made an unfiltered tail stop five minutes into a sixty-minute
	// window and return the wrong end of it (see scan). Scan volume is
	// bounded by MaxEventsScanned and MaxCalls, which is where that
	// belongs.
	defaultMaxBytes = 2 << 20 // 2 MiB

	// defaultMaxMessageBytes truncates one oversized line rather than
	// letting a single stack trace consume the page's byte budget.
	defaultMaxMessageBytes = 16 << 10

	// perCallLimit is FilterLogEvents' own page size. Large on purpose:
	// the budget that matters is events SCANNED, and a bigger page
	// scans the same events in fewer round trips.
	perCallLimit = 10000

	// minCallsPerTarget guarantees every resolved log group gets read
	// even when several share the budget. A group silently skipped
	// because an earlier one exhausted the allowance is indistinguishable
	// from a group with no matching lines.
	//
	// Four, not two: the backward chunk walk spends a call per chunk, so
	// a two-call allowance would cover only the newest ~2.5 minutes of a
	// quiet group's window and report a cap for the rest.
	minCallsPerTarget = 4

	// The backward scan's chunk sizes, anchored at the window's end and
	// growing as the walk moves older. Small first chunk so a busy group
	// answers a tail in one call; ×8 growth so a quiet group still
	// reaches 48h in ~6 chunks instead of spending the call budget on
	// empty intervals.
	firstChunk  = 30 * time.Second
	chunkGrowth = 8
	maxChunk    = 12 * time.Hour
)

// New builds the provider. It does NOT verify the credential — that is
// Health()'s job, and failing construction would take down the whole
// dashboard (registry, deploy history, health) over a telemetry
// permission the operator may not have granted yet.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.DefaultRegion == "" {
		return nil, errors.New("awsprov: DefaultRegion is required")
	}
	if cfg.GroupCacheTTL == 0 {
		cfg.GroupCacheTTL = defaultGroupCacheTTL
	}
	p := &Provider{
		cfg:      cfg,
		clients:  map[string]cwAPI{},
		mclients: map[string]cwmAPI{},
		groups:   map[string]groupCacheEntry{},
		metrics:  map[string]metricCacheEntry{},
	}
	p.clientFor = p.realClient
	p.metricClientFor = p.realMetricClient
	// Touch ctx so the signature stays honest about doing I/O later and
	// callers keep passing a cancellable context in.
	_ = ctx
	return p, nil
}

func (p *Provider) now() time.Time {
	if p.cfg.Now == nil {
		return time.Now().UTC()
	}
	return p.cfg.Now().UTC()
}

func (p *Provider) Name() string { return registry.CloudAWS }

// Capabilities reports logs and metrics. Workloads is wired into the
// interface but refuses — so the dashboard says "not implemented yet"
// instead of drawing a replica count of zero.
func (p *Provider) Capabilities() telemetry.Capabilities {
	return telemetry.Capabilities{Logs: true, Metrics: true, Workloads: false}
}

func (p *Provider) Workloads(context.Context, telemetry.WorkloadQuery) ([]telemetry.Workload, error) {
	return nil, telemetry.Unsupported("the AWS provider", "live workload state")
}

// Health proves the credential can list log groups. Cheap, and the
// cheapest call that actually exercises the grant the dashboard needs —
// an STS GetCallerIdentity would succeed with no logs permission at all
// and report a healthy provider that cannot read anything.
func (p *Provider) Health(ctx context.Context) error {
	cl, err := p.client(ctx, p.cfg.DefaultRegion)
	if err != nil {
		return err
	}
	if _, err := cl.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
		Limit: aws.Int32(1),
	}); err != nil {
		return fmt.Errorf("cloudwatch logs unreachable in %s: %w", p.cfg.DefaultRegion, err)
	}
	return nil
}

// target is one concrete log group to read, in one region.
type target struct {
	region string
	group  string
	// from records which handle produced it, so a prefix that resolved
	// to several groups stays attributable in the result.
	from string
}

// Logs reads one page, newest-first, bounded by the window and the
// budget.
//
// The page is the NEWEST `Limit` events in the window, which needs a
// word of explanation: CloudWatch's FilterLogEvents only walks a window
// forward, so there is no "last N" call to make. scan() therefore reads
// the window newest-sub-window-first and stops as soon as it holds a
// full page — see its comment for why the straightforward forward walk
// returned the wrong end of a busy window. When the budget stops even
// that early, CapReason names the interval that was read and which end
// of it the events come from: presenting the oldest part of a window as
// "the latest logs" would be the dashboard lying about its own axis.
func (p *Provider) Logs(ctx context.Context, q telemetry.LogQuery) (telemetry.LogPage, error) {
	page := telemetry.LogPage{
		Events:   []telemetry.LogEvent{},
		Queried:  []string{},
		Warnings: []string{},
	}
	budget := applyBudgetDefaults(q.Budget)

	start, end := q.Start, q.End
	if start.IsZero() || end.IsZero() || !start.Before(end) {
		return page, &telemetry.UnavailableError{Reason: "the log query carries no usable time window"}
	}
	exclude := map[string]bool{}
	if q.Cursor != "" {
		c, err := telemetry.DecodeCursor(q.Cursor)
		if err != nil {
			return page, fmt.Errorf("bad cursor: %w", err)
		}
		// Re-query the boundary millisecond and drop what was already
		// delivered; see telemetry.Cursor for why neither `<` nor `<=`
		// is correct on its own.
		if b := c.Before().Add(time.Millisecond); b.Before(end) {
			end = b
		}
		for _, id := range c.ExcludeIDs {
			exclude[id] = true
		}
		if !start.Before(end) {
			// The cursor walked back past the window floor: there is
			// nothing older to serve. An empty page with no cursor is
			// the correct terminal answer.
			return page, nil
		}
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	targets, warns := p.resolveTargets(ctx, q.Sources)
	page.Warnings = append(page.Warnings, warns...)
	if len(targets) == 0 {
		if len(page.Warnings) > 0 {
			// Resolution ran and found nothing — that is a REASON, not
			// an empty result. The common case is an inferred prefix
			// that does not match any live group, which looks exactly
			// like a quiet service unless it is said out loud.
			return page, &telemetry.UnavailableError{
				Reason: "no log group could be resolved for this service: " + strings.Join(page.Warnings, "; "),
			}
		}
		return page, &telemetry.UnavailableError{
			Reason: "this service has no CloudWatch log handles recorded",
		}
	}

	// Split the budget across targets rather than letting the first one
	// consume it. A log group skipped for lack of allowance returns the
	// same empty list as a log group with nothing in it.
	//
	// MaxBytes is deliberately NOT split: it bounds the one response
	// these targets jointly produce, and is applied once to the merged
	// page below.
	perTarget := budget
	if n := len(targets); n > 1 {
		perTarget.MaxCalls = max(budget.MaxCalls/n, minCallsPerTarget)
		perTarget.MaxEventsScanned = max(budget.MaxEventsScanned/n, 1)
	}

	var (
		merged   []telemetry.LogEvent
		failed   int
		notFound int
		lastErr  error
	)
	for _, t := range targets {
		page.Queried = append(page.Queried, t.group)
		res, err := p.scan(ctx, t, q.Filter, start, end, limit, perTarget, exclude)
		page.Scanned += res.scanned
		page.Calls += res.calls
		if err != nil {
			failed++
			if isNotFound(err) {
				notFound++
			}
			lastErr = err
			page.Warnings = append(page.Warnings, fmt.Sprintf("%s: %s", t.group, errText(err)))
			continue
		}
		if res.capped {
			page.Capped = true
			page.CapReason = res.capReason
		}
		merged = append(merged, res.events...)
	}

	// Every target failed: that is a failure, not a quiet service, and
	// it must surface as an error rather than as an empty page with a
	// warning nobody reads. The most common cause is a missing grant,
	// and the message carries the AWS text verbatim so it is actionable.
	if failed == len(targets) {
		// One exception, and it is not a fault: the groups simply do not
		// exist. An EXACT handle (the Lambda access-logs group) has no
		// resolution step to catch that, so the absence arrives here as
		// a ResourceNotFoundException from the read itself. Reporting
		// "log query failed" for it would dress a derivation that names
		// a group nobody created as a broken dashboard.
		//
		// ALL of them, not just the last: one denied group alongside one
		// missing group is still a permissions problem, and softening it
		// to "unavailable" would hide the grant that needs fixing.
		if notFound == failed {
			return page, &telemetry.UnavailableError{Reason: fmt.Sprintf(
				"no such log group in %s: %s — nothing has ever written there, so the recorded handle may not match what the template created",
				targets[0].region, strings.Join(page.Queried, ", "),
			)}
		}
		return page, fmt.Errorf("every log group failed to read: %w", lastErr)
	}

	// Newest first. Stable on the id so a page boundary is deterministic
	// across requests — without it, two events sharing a millisecond can
	// swap order between pages and one of them is then either shown
	// twice or skipped by the cursor.
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].At.Equal(merged[j].At) {
			return merged[i].ID > merged[j].ID
		}
		return merged[i].At.After(merged[j].At)
	})
	full := len(merged) >= limit
	if len(merged) > limit {
		merged = merged[:limit]
	}

	// The response-size budget, applied to what is RETURNED. merged is
	// newest-first, so trimming the tail keeps the newest events and
	// drops the oldest — the same direction "load older" continues in.
	if trimmed, dropped := trimToBytes(merged, budget.MaxBytes); dropped > 0 {
		merged = trimmed
		full = true // there IS older data; the cursor below must say so
		page.Capped = true
		page.CapReason = fmt.Sprintf(
			"the page was trimmed to its newest %d events (about %d KiB): %d older events that were read did not fit the response-size cap — page back with the cursor, or narrow the window",
			len(merged), budget.MaxBytes/1024, dropped,
		)
	}
	page.Events = merged

	// A cursor only means something when the page filled: a short page
	// means the window was exhausted, and handing back a cursor there
	// would invite an endless "load more" that always returns nothing.
	// A byte-trimmed page counts as filled even when it is short — the
	// events it dropped are exactly what the next page must serve.
	if full && len(merged) > 0 {
		page.NextCursor = telemetry.CursorFor(merged)
	}
	return page, nil
}

// trimToBytes drops events from the OLD end until the page fits the
// response budget, returning the kept slice and how many went.
//
// At least one event always survives: a single line longer than the
// whole budget is already truncated to MaxMessageBytes, and returning
// an empty page for it would report "the window was quiet".
func trimToBytes(events []telemetry.LogEvent, maxBytes int) ([]telemetry.LogEvent, int) {
	if maxBytes <= 0 {
		return events, 0
	}
	total := 0
	for i := range events {
		total += len(events[i].Message)
		if total > maxBytes && i > 0 {
			return events[:i], len(events) - i
		}
	}
	return events, 0
}

type scanResult struct {
	events    []telemetry.LogEvent
	scanned   int
	calls     int
	capped    bool
	capReason string
}

// stopReason names which of the two scan limits ran out. Neither is
// something a reader can raise, but they lead to different reads of the
// same page: the call cap means the group pages slowly, the scan cap
// means it is loud, and "add a filter" helps the second much more than
// the first.
func (r *scanResult) stopReason(budget telemetry.Budget) string {
	if r.scanned >= budget.MaxEventsScanned {
		return fmt.Sprintf("the scan cap stopped this read after %d events", r.scanned)
	}
	return fmt.Sprintf("the per-request call cap stopped this read after %d CloudWatch calls", r.calls)
}

// scan reads the NEWEST `limit` events of one log group's window.
//
// # Why this walks the window in chunks instead of straight through
//
// FilterLogEvents has exactly one direction: forward from StartTime.
// The first cut therefore walked the whole window forward and kept the
// newest `limit` events in a ring — correct when the walk completes,
// and wrong in a specific, bad way when the budget stops it early: the
// events held are then the newest of the window's OLDEST part, while
// the panel's header still says the window ends now.
//
// Live-caught 2026-11-22 on monitor.simple-forge.com: an unfiltered
// 60-minute tail of forge-conductor--staging capped three calls in and
// returned events from 12:16 under a window labelled `-> 13:12`. The
// cap reason said so, so nothing was silently wrong — but "tail this
// service" is the panel's main use and it showed an hour-stale page by
// default. (With a filter it was always right: fewer matches meant the
// walk reached the window's end.)
//
// So: cut the window into sub-windows and scan the NEWEST one first,
// growing older chunks geometrically. Within a chunk the walk is still
// forward — it just finishes. Once the ring holds `limit` events the
// scan stops, because every event not yet read lies in an older chunk
// and therefore cannot displace anything held. A busy service now
// answers from the first 30-second chunk in one or two calls; a quiet
// one pays ~6 calls to cover 48 hours, which is what the call budget is
// for.
func (p *Provider) scan(
	ctx context.Context, t target, filter string,
	start, end time.Time, limit int,
	budget telemetry.Budget, exclude map[string]bool,
) (scanResult, error) {
	var res scanResult
	cl, err := p.client(ctx, t.region)
	if err != nil {
		return res, err
	}

	keep := newRing(limit)
	chunk := firstChunk
	cur := end

	for cur.After(start) {
		chunkStart := cur.Add(-chunk)
		if chunkStart.Before(start) {
			chunkStart = start
		}
		done, err := p.scanChunk(ctx, cl, t, filter, chunkStart, cur, keep, &res, budget, exclude)
		if err != nil {
			return res, err
		}
		if !done {
			// The budget ran out INSIDE this chunk, so the ring holds the
			// oldest events of it rather than the newest. Say which
			// interval was read and which end of it this is — the reader
			// cannot tell from the timestamps alone, and a filter is the
			// fix rather than a wider window.
			res.capped = true
			res.capReason = fmt.Sprintf(
				"%s inside %s–%s, which this log group fills faster than one request may read: these are the OLDEST events of that interval and NOT of the whole window — add a filter to narrow what is scanned",
				res.stopReason(budget),
				chunkStart.Format(time.RFC3339), cur.Format(time.RFC3339),
			)
			break
		}
		if keep.len() >= limit {
			// Everything still unread is older than everything held, so
			// the page is already the window's true newest. Not a cap.
			break
		}
		cur = chunkStart
		if chunk <= maxChunk/chunkGrowth {
			chunk *= chunkGrowth
		} else {
			chunk = maxChunk
		}
	}

	res.events = keep.items()
	return res, nil
}

// scanChunk walks one sub-window forward to exhaustion, pushing what it
// reads into keep. It reports whether the sub-window was FINISHED — a
// false means the shared budget stopped it part-way, which the caller
// has to describe differently from a completed chunk because the ring
// then holds the wrong end of the interval.
//
// Budget counters live on res and are shared across chunks on purpose:
// the cap bounds one HTTP request's cost, not one chunk's.
func (p *Provider) scanChunk(
	ctx context.Context, cl cwAPI, t target, filter string,
	start, end time.Time, keep *ring, res *scanResult,
	budget telemetry.Budget, exclude map[string]bool,
) (bool, error) {
	in := &cloudwatchlogs.FilterLogEventsInput{
		LogGroupName: aws.String(t.group),
		StartTime:    aws.Int64(start.UnixMilli()),
		EndTime:      aws.Int64(end.UnixMilli()),
		Limit:        aws.Int32(perCallLimit),
	}
	if filter != "" {
		in.FilterPattern = aws.String(filter)
	}

	var token *string
	for {
		if res.calls >= budget.MaxCalls || res.scanned >= budget.MaxEventsScanned {
			return false, nil
		}
		in.NextToken = token
		out, err := cl.FilterLogEvents(ctx, in)
		res.calls++
		if err != nil {
			return false, err
		}
		for i := range out.Events {
			ev := toEvent(out.Events[i], t.from, budget.MaxMessageBytes)
			res.scanned++
			if ev.ID != "" && exclude[ev.ID] {
				continue
			}
			keep.push(ev)
		}
		if out.NextToken == nil || *out.NextToken == "" {
			return true, nil
		}
		token = out.NextToken
	}
}

// resolveTargets turns handles into concrete log groups, expanding the
// prefix handles that the inference fallback produces.
//
// Returns warnings rather than errors for individual handles: one
// unresolvable handle out of three must not sink the other two, and
// must also not vanish.
func (p *Provider) resolveTargets(ctx context.Context, sources []registry.TelemetrySource) ([]target, []string) {
	var (
		targets []target
		warns   []string
		seen    = map[string]bool{}
	)
	for _, s := range sources {
		if s.Kind != SourceKindLogs {
			continue
		}
		region := s.Region
		if region == "" {
			region = p.cfg.DefaultRegion
		}
		if s.Ref == "" {
			warns = append(warns, "a recorded log handle has no reference")
			continue
		}
		if s.Dims[DimMatch] != DimMatchPrefix {
			key := region + "|" + s.Ref
			if !seen[key] {
				seen[key] = true
				targets = append(targets, target{region: region, group: s.Ref, from: s.Ref})
			}
			continue
		}
		groups, err := p.resolvePrefix(ctx, region, s.Ref)
		if err != nil {
			warns = append(warns, fmt.Sprintf("resolving log-group prefix %q: %s", s.Ref, errText(err)))
			continue
		}
		if len(groups) == 0 {
			// The single most important message in this file. An
			// inferred prefix that matches nothing is a WRONG GUESS,
			// and the symptom — an empty log view — is identical to a
			// service that simply had nothing to say.
			warns = append(warns, fmt.Sprintf(
				"no log group starts with %q in %s (this handle was inferred from naming conventions, so the guess may be wrong)",
				s.Ref, region,
			))
			continue
		}
		for _, g := range groups {
			key := region + "|" + g
			if !seen[key] {
				seen[key] = true
				targets = append(targets, target{region: region, group: g, from: s.Ref})
			}
		}
	}
	return targets, warns
}

func (p *Provider) resolvePrefix(ctx context.Context, region, prefix string) ([]string, error) {
	key := region + "|" + prefix
	p.mu.Lock()
	if e, ok := p.groups[key]; ok && p.now().Sub(e.at) < p.cfg.GroupCacheTTL {
		p.mu.Unlock()
		return e.groups, nil
	}
	p.mu.Unlock()

	cl, err := p.client(ctx, region)
	if err != nil {
		return nil, err
	}
	var (
		out   []string
		token *string
	)
	// Two pages at most. A prefix matching more than 100 log groups is
	// a sign the handle is too broad to be useful, and walking it all
	// would spend the request's time budget before a single log line is
	// read.
	for i := 0; i < 2; i++ {
		res, err := cl.DescribeLogGroups(ctx, &cloudwatchlogs.DescribeLogGroupsInput{
			LogGroupNamePrefix: aws.String(prefix),
			Limit:              aws.Int32(50),
			NextToken:          token,
		})
		if err != nil {
			return nil, err
		}
		for _, g := range res.LogGroups {
			if n := aws.ToString(g.LogGroupName); n != "" {
				out = append(out, n)
			}
		}
		if res.NextToken == nil || *res.NextToken == "" {
			break
		}
		token = res.NextToken
	}
	sort.Strings(out)

	p.mu.Lock()
	p.groups[key] = groupCacheEntry{groups: out, at: p.now()}
	p.mu.Unlock()
	return out, nil
}

func (p *Provider) client(ctx context.Context, region string) (cwAPI, error) {
	if region == "" {
		region = p.cfg.DefaultRegion
	}
	p.mu.Lock()
	if c, ok := p.clients[region]; ok {
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	c, err := p.clientFor(ctx, region)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.clients[region] = c
	p.mu.Unlock()
	return c, nil
}

func (p *Provider) realClient(ctx context.Context, region string) (cwAPI, error) {
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
	return cloudwatchlogs.NewFromConfig(awsCfg), nil
}

func toEvent(e cwtypes.FilteredLogEvent, source string, maxMessage int) telemetry.LogEvent {
	out := telemetry.LogEvent{
		Message: aws.ToString(e.Message),
		Stream:  aws.ToString(e.LogStreamName),
		Source:  source,
		ID:      aws.ToString(e.EventId),
	}
	if e.Timestamp != nil {
		out.At = time.UnixMilli(*e.Timestamp).UTC()
	}
	// CloudWatch keeps the trailing newline the runtime wrote; the UI
	// renders one event per row, so it is noise everywhere.
	out.Message = strings.TrimRight(out.Message, "\r\n")
	if maxMessage > 0 && len(out.Message) > maxMessage {
		out.Message = out.Message[:maxMessage]
		out.Truncated = true
	}
	return out
}

func applyBudgetDefaults(b telemetry.Budget) telemetry.Budget {
	if b.MaxCalls <= 0 {
		b.MaxCalls = defaultMaxCalls
	}
	if b.MaxEventsScanned <= 0 {
		b.MaxEventsScanned = defaultMaxEventsScanned
	}
	if b.MaxBytes <= 0 {
		b.MaxBytes = defaultMaxBytes
	}
	if b.MaxMessageBytes <= 0 {
		b.MaxMessageBytes = defaultMaxMessageBytes
	}
	return b
}

// errText keeps an AWS error readable in a warning line. The SDK's
// Error() carries the operation, the request id and the HTTP status on
// one very long line; the last segment is the part an operator acts on
// (AccessDeniedException, ResourceNotFoundException).
// isNotFound reports whether a read failed because the log group does
// not exist, as opposed to any other reason.
//
// Matched on the typed error, not on the message: `errors.As` against
// cwtypes.ResourceNotFoundException survives AWS rewording its text,
// and the alternative — a string match on "ResourceNotFound" — would
// also swallow an IAM denial whose message happened to quote it.
func isNotFound(err error) bool {
	var nf *cwtypes.ResourceNotFoundException
	return errors.As(err, &nf)
}

func errText(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i > 0 && i < len(s)-2 {
		return s[i+2:]
	}
	return s
}

// ring keeps the newest N events seen, in insertion order.
//
// A ring rather than "collect everything and slice the tail": the scan
// cap allows 20k events, and holding all of them to return 200 wastes
// the memory of a 512 MB Lambda that is also holding a config snapshot.
type ring struct {
	buf   []telemetry.LogEvent
	start int
	count int
}

func newRing(size int) *ring {
	if size < 1 {
		size = 1
	}
	return &ring{buf: make([]telemetry.LogEvent, size)}
}

func (r *ring) push(e telemetry.LogEvent) {
	if r.count < len(r.buf) {
		r.buf[(r.start+r.count)%len(r.buf)] = e
		r.count++
		return
	}
	r.buf[r.start] = e
	r.start = (r.start + 1) % len(r.buf)
}

// len is how many events are held, which is what tells the backward
// scan it may stop: a full ring cannot be displaced by anything older.
func (r *ring) len() int { return r.count }

func (r *ring) items() []telemetry.LogEvent {
	out := make([]telemetry.LogEvent, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.buf[(r.start+i)%len(r.buf)])
	}
	return out
}
