// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/notify"
	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

var probeAt = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

// fakePublisher records what would have been published, so the edge →
// kind+severity mapping can be asserted without an HTTP server.
type fakePublisher struct {
	enabled bool
	err     error
	sent    []notify.Event
	linked  []string
}

func (f *fakePublisher) Publish(_ context.Context, ev notify.Event) error {
	f.sent = append(f.sent, ev)
	return f.err
}

func (f *fakePublisher) Enabled() bool { return f.enabled }

func (f *fakePublisher) RunLink(path string) string {
	f.linked = append(f.linked, path)
	return "https://monitor.example.com" + path
}

// brokenLatest fails only LatestHealth; everything else behaves. The
// store interface has no other way to simulate a partial outage.
type brokenLatest struct {
	registry.Store
}

func (brokenLatest) LatestHealth(context.Context, string) (map[string]registry.HealthResult, error) {
	return nil, errors.New("storage unavailable")
}

func result(stack, region, status string) registry.HealthResult {
	return registry.HealthResult{Stack: stack, Region: region, Status: status, At: probeAt}
}

func seed(t *testing.T, store registry.Store, results ...registry.HealthResult) {
	t.Helper()
	for _, res := range results {
		if err := store.PutHealth(context.Background(), res); err != nil {
			t.Fatalf("seed PutHealth: %v", err)
		}
	}
}

// The edge table. Each row is a noise class this component would emit
// if the comparison were written the obvious way.
func TestDetectTransitions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prior []registry.HealthResult
		fresh []registry.HealthResult
		want  []Transition
	}{
		{
			// The first sweep after a fresh deploy. With no prior, an
			// "edge" would fire for every service in the fleet at once —
			// wrong (nothing changed; we only just started looking) and a
			// storm on the component's first minute of life.
			name:  "no prior result is never an edge",
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthDown)},
		},
		{
			// The row that actually bites. An absent prior reads as the
			// zero value, which is not-up — so dropping the guard looks
			// harmless against a DOWN result (not-up → not-up, no edge)
			// and emits "Service recovered" for every healthy service in
			// the fleet on the first sweep. Found by inverting the guard:
			// the down case above passed without it.
			name:  "no prior result is never an edge, not even into up",
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUp)},
		},
		{
			name:  "a prior with an empty status is treated as absent",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", "")},
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthDown)},
		},
		{
			name:  "a prior with an empty status is absent in the up direction too",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", "")},
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUp)},
		},
		{
			name:  "up to down",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUp)},
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthDown)},
			want: []Transition{
				{Stack: "forge-baas", Region: "us-east-1", From: registry.HealthUp, To: registry.HealthDown},
			},
		},
		{
			name:  "down to up",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthDown)},
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUp)},
			want: []Transition{
				{Stack: "forge-baas", Region: "us-east-1", From: registry.HealthDown, To: registry.HealthUp},
			},
		},
		{
			// The level, every minute, for as long as the outage lasts.
			// Emitting here is 1,440 identical messages a day and a muted
			// channel by lunchtime.
			name:  "still down is not an edge",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthDown)},
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthDown)},
		},
		{
			name:  "still up is not an edge",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUp)},
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUp)},
		},
		{
			// `unknown` collapses onto the not-up axis, which removes the
			// down↔unknown flap class entirely. A service alternating
			// between "refused the connection" and "could not classify"
			// is ONE outage, not an edge per tick.
			name:  "down to unknown is the same outage",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthDown)},
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUnknown)},
		},
		{
			name:  "up to unknown is an edge",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUp)},
			fresh: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUnknown)},
			want: []Transition{
				{Stack: "forge-baas", Region: "us-east-1", From: registry.HealthUp, To: registry.HealthUnknown},
			},
		},
		{
			// Regions are independent: a multi-region service with one
			// bad region must report that region, and must not report the
			// healthy one.
			name: "regions are compared independently",
			prior: []registry.HealthResult{
				result("forge-baas", "us-east-1", registry.HealthUp),
				result("forge-baas", "eu-west-1", registry.HealthUp),
			},
			fresh: []registry.HealthResult{
				result("forge-baas", "us-east-1", registry.HealthUp),
				result("forge-baas", "eu-west-1", registry.HealthDown),
			},
			want: []Transition{
				{Stack: "forge-baas", Region: "eu-west-1", From: registry.HealthUp, To: registry.HealthDown},
			},
		},
		{
			// A new region appearing on an already-known stack is still an
			// absent prior for THAT region.
			name:  "a newly probed region of a known stack is not an edge",
			prior: []registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthUp)},
			fresh: []registry.HealthResult{result("forge-baas", "eu-west-1", registry.HealthDown)},
		},
		{
			name: "two stacks both flip",
			prior: []registry.HealthResult{
				result("forge-baas", "us-east-1", registry.HealthUp),
				result("forge-storage", "us-east-1", registry.HealthDown),
			},
			fresh: []registry.HealthResult{
				result("forge-baas", "us-east-1", registry.HealthDown),
				result("forge-storage", "us-east-1", registry.HealthUp),
			},
			want: []Transition{
				{Stack: "forge-baas", Region: "us-east-1", From: registry.HealthUp, To: registry.HealthDown},
				{Stack: "forge-storage", Region: "us-east-1", From: registry.HealthDown, To: registry.HealthUp},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := registry.NewInMemoryStore()
			seed(t, store, tc.prior...)

			got := DetectTransitions(context.Background(), store, tc.fresh)

			if len(got) != len(tc.want) {
				t.Fatalf("got %d transitions, want %d:\n  %+v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if got[i].Stack != want.Stack || got[i].Region != want.Region ||
					got[i].From != want.From || got[i].To != want.To {
					t.Errorf("transition %d = %+v, want stack=%s region=%s %s→%s",
						i, got[i], want.Stack, want.Region, want.From, want.To)
				}
				if !got[i].At.Equal(probeAt) {
					t.Errorf("transition %d At = %v, want the probe's own timestamp %v", i, got[i].At, probeAt)
				}
			}
		})
	}
}

// A storage read failure must not fabricate an edge. Reporting "down"
// because the prior could not be read would page the operator about the
// observatory itself; swallowing it at worst delays one real
// notification by one tick.
func TestDetectTransitionsSwallowsAStoreError(t *testing.T) {
	store := brokenLatest{registry.NewInMemoryStore()}

	got := DetectTransitions(context.Background(), store,
		[]registry.HealthResult{result("forge-baas", "us-east-1", registry.HealthDown)})

	if len(got) != 0 {
		t.Errorf("an unreadable prior must produce no edge, got %+v", got)
	}
}

// One read per stack, not per result. A fleet-wide sweep of a
// multi-region service would otherwise issue one storage read per
// region per tick.
func TestDetectTransitionsReadsEachStackOnce(t *testing.T) {
	store := registry.NewInMemoryStore()
	seed(
		t, store,
		result("forge-baas", "us-east-1", registry.HealthUp),
		result("forge-baas", "eu-west-1", registry.HealthUp),
	)
	counting := &countingLatest{Store: store}

	DetectTransitions(context.Background(), counting, []registry.HealthResult{
		result("forge-baas", "us-east-1", registry.HealthDown),
		result("forge-baas", "eu-west-1", registry.HealthDown),
		result("forge-baas", "ap-south-1", registry.HealthDown),
	})

	if counting.calls != 1 {
		t.Errorf("LatestHealth called %d times for one stack, want 1", counting.calls)
	}
}

type countingLatest struct {
	registry.Store
	calls int
}

func (c *countingLatest) LatestHealth(ctx context.Context, stack string) (map[string]registry.HealthResult, error) {
	c.calls++
	return c.Store.LatestHealth(ctx, stack)
}

// The severity mapping, which is the thing most likely to be quietly
// wrong. `warning` is every channel's default minimum, so recovery
// mapped to `info` is a notification nobody ever receives — in
// forge-notifier every Telegram subscription was `severityAtLeast:
// warning` and every `info` notification silently vanished for months.
// An operator paged about an outage has to see it end.
func TestTransitionEventSeverityAndTitle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		to       string
		severity string
		title    string
	}{
		{"down", registry.HealthDown, notify.SeverityError, "Service down: forge-baas/us-east-1"},
		{"unknown", registry.HealthUnknown, notify.SeverityError, "Service down: forge-baas/us-east-1"},
		{"up", registry.HealthUp, notify.SeverityWarning, "Service recovered: forge-baas/us-east-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePublisher{enabled: true}
			tr := Transition{
				Stack: "forge-baas", Region: "us-east-1",
				From: registry.HealthUp, To: tc.to, At: probeAt,
				Result: result("forge-baas", "us-east-1", tc.to),
			}

			ev := transitionEvent(fake, tr)

			if ev.Severity != tc.severity {
				t.Errorf("Severity = %q, want %q", ev.Severity, tc.severity)
			}
			if ev.Title != tc.title {
				t.Errorf("Title = %q, want %q", ev.Title, tc.title)
			}
			if ev.Kind != notify.KindHealthChanged {
				t.Errorf("Kind = %q", ev.Kind)
			}
			if ev.ID != notify.HealthEdgeEventID("forge-baas", "us-east-1", tc.to, probeAt) {
				t.Errorf("ID = %q — must be the edge-derived idempotency key", ev.ID)
			}
			if ev.Source != "forge-baas" || ev.Subject != "forge-baas/us-east-1" {
				t.Errorf("Source/Subject = %q/%q", ev.Source, ev.Subject)
			}
			if !ev.At.Equal(probeAt) {
				t.Errorf("At = %v, want the probe timestamp", ev.At)
			}
			if ev.URL != "https://monitor.example.com/#/services/forge-baas" {
				t.Errorf("URL = %q — the operator taps this from the message", ev.URL)
			}
			if ev.Labels["stack"] != "forge-baas" || ev.Labels["region"] != "us-east-1" ||
				ev.Labels["from"] != registry.HealthUp || ev.Labels["to"] != tc.to {
				t.Errorf("Labels = %v", ev.Labels)
			}
		})
	}
}

// A single-region service has no region to name, and the subject must
// not grow a dangling slash.
func TestTransitionEventSubjectWithoutRegion(t *testing.T) {
	fake := &fakePublisher{enabled: true}
	ev := transitionEvent(fake, Transition{
		Stack: "forge-baas", From: registry.HealthUp, To: registry.HealthDown, At: probeAt,
	})
	if ev.Subject != "forge-baas" {
		t.Errorf("Subject = %q, want the bare stack", ev.Subject)
	}
	if strings.HasSuffix(ev.Title, "/") {
		t.Errorf("Title = %q has a dangling separator", ev.Title)
	}
}

// The body is what the operator actually reads in Telegram; they have
// no other view of the diagnosis from there. A transport failure and an
// HTTP 500 are different faults and must not collapse — HTTPStatus is
// zero for the first, so the error line is its only evidence.
func TestTransitionBodyCarriesTheDiagnosis(t *testing.T) {
	body := transitionBody(Transition{
		From: registry.HealthUp, To: registry.HealthDown,
		Result: registry.HealthResult{
			HTTPStatus: 502, LatencyMS: 1234,
			Error: "dial tcp: connection refused", Version: "2026.11.23-abc1234",
		},
	})
	for _, want := range []string{"up → down", "http: 502", "latency: 1234ms", "connection refused", "2026.11.23-abc1234"} {
		if !strings.Contains(body, want) {
			t.Errorf("body does not mention %q:\n%s", want, body)
		}
	}

	// A transport failure has no HTTP status, and a zero must not be
	// rendered as though the service answered 0.
	body = transitionBody(Transition{
		From: registry.HealthUp, To: registry.HealthUnknown,
		Result: registry.HealthResult{Error: "context deadline exceeded"},
	})
	if strings.Contains(body, "http:") {
		t.Errorf("a status-less probe must not render an http line:\n%s", body)
	}
	if !strings.Contains(body, "deadline exceeded") {
		t.Errorf("the transport error is the only diagnosis there is:\n%s", body)
	}
}

// With no notifier configured the publish step is a no-op, and must not
// even build an event — RunLink included, since that is observable.
func TestPublishTransitionsSkipsADisabledPublisher(t *testing.T) {
	fake := &fakePublisher{enabled: false}
	tr := []Transition{{Stack: "forge-baas", From: registry.HealthUp, To: registry.HealthDown, At: probeAt}}

	PublishTransitions(context.Background(), fake, tr)

	if len(fake.sent) != 0 || len(fake.linked) != 0 {
		t.Errorf("a disabled publisher must not be used: sent=%d linked=%v", len(fake.sent), fake.linked)
	}

	// Nil is the default on every stack that has not configured one.
	PublishTransitions(context.Background(), nil, tr)
}

// A publish failure must not stop the remaining edges. A notifier
// refusing one event is not a reason to lose the other three outages
// the same sweep found.
func TestPublishTransitionsContinuesPastAnError(t *testing.T) {
	fake := &fakePublisher{enabled: true, err: errors.New("notifier down")}

	PublishTransitions(context.Background(), fake, []Transition{
		{Stack: "a", From: registry.HealthUp, To: registry.HealthDown, At: probeAt},
		{Stack: "b", From: registry.HealthUp, To: registry.HealthDown, At: probeAt},
	})

	if len(fake.sent) != 2 {
		t.Errorf("attempted %d publishes, want 2", len(fake.sent))
	}
}

// The ordering contract, proven end to end through Sweep rather than
// asserted about DetectTransitions in isolation: edges are detected
// BEFORE the persist loop. Once PutHealth has run, LatestHealth returns
// the result we just wrote and every edge has been erased — so a Sweep
// that persisted first would be permanently silent, which is exactly
// the bug this slice exists to fix.
func TestSweepDetectsTheEdgeBeforePersisting(t *testing.T) {
	var up bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if up {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := registry.NewInMemoryStore()
	if _, err := registry.Register(context.Background(), store, registry.ServiceRecord{
		Stack: "edge--staging", EndpointURL: srv.URL, HealthPath: "/healthz",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("register: %v", err)
	}

	fake := &fakePublisher{enabled: true}
	cfg := SweepConfig{DefaultPath: "/healthz", Events: fake}
	sweep := func() SweepOutcome {
		t.Helper()
		out, err := Sweep(context.Background(), store, NewHTTPProber(2*time.Second), cfg)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		return out
	}

	// First sweep: down, but with no prior there is no edge.
	if out := sweep(); len(out.Transitions) != 0 {
		t.Fatalf("first sweep must emit no edge, got %+v", out.Transitions)
	}
	// Second sweep: still down. The level repeats; the edge does not.
	if out := sweep(); len(out.Transitions) != 0 {
		t.Fatalf("a repeated level must emit no edge, got %+v", out.Transitions)
	}
	// Third: recovery. Exactly one edge, and it reached the publisher.
	up = true
	out := sweep()
	if len(out.Transitions) != 1 || !out.Transitions[0].Recovered() {
		t.Fatalf("recovery must emit exactly one up edge, got %+v", out.Transitions)
	}
	if len(fake.sent) != 1 {
		t.Fatalf("publisher saw %d events, want 1", len(fake.sent))
	}
	if fake.sent[0].Severity != notify.SeverityWarning {
		t.Errorf("recovery severity = %q, want warning", fake.sent[0].Severity)
	}
	// Fourth: still up. Silence again.
	if out := sweep(); len(out.Transitions) != 0 {
		t.Errorf("a settled service must emit no edge, got %+v", out.Transitions)
	}
	if len(fake.sent) != 1 {
		t.Errorf("publisher saw %d events in total, want 1 — one outage, one recovery message", len(fake.sent))
	}
}
