// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package health

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/notify"
	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// This file is the whole reason the sweep is worth running on a timer.
//
// `Sweep` has recorded `up|down|unknown` per service per region every
// five minutes since the component shipped, and nothing ever diffed two
// consecutive results — so an up→down EDGE did not exist anywhere in
// the system, and an operator learned about an outage by opening the
// SPA. The level was stored; the event was not.
//
// The distinction is not pedantic. Notifying on the LEVEL means a
// message every tick for as long as the service is down — 288 identical
// "still down" messages a day at a 5-minute interval — which an
// operator mutes within the hour, taking the real alerts with it.
// Notifying on the EDGE means exactly one message when it breaks and
// one when it comes back, which is the plan's definition of done.

// Transition is one health edge: a service crossed between up and
// not-up, per region.
type Transition struct {
	Stack  string `json:"stack"`
	Region string `json:"region,omitempty"`

	// From and To are the recorded statuses either side of the edge.
	From string `json:"from"`
	To   string `json:"to"`

	// At is the time of the probe that OBSERVED the new state, not the
	// time this transition was computed. It is part of the event's
	// idempotency key, so it has to be the probe's own timestamp for two
	// overlapping sweeps to dedupe.
	At time.Time `json:"at"`

	// Result is the probe that observed the new state, carried so the
	// message body can name the HTTP status or transport error without
	// a second lookup.
	Result registry.HealthResult `json:"-"`
}

// Recovered reports whether this edge is a return to health.
func (t Transition) Recovered() bool { return isUp(t.To) }

// isUp collapses the three recorded statuses onto the one axis an
// operator is paged about.
//
// `unknown` counts as not-up, and that is a deliberate simplification:
// it means the probe could not answer the question (no endpoint
// published, a transport failure the prober could not classify), which
// is not a healthy service. Collapsing it also removes a whole class of
// noise — a service flapping between `down` and `unknown` is one
// outage, and treating those as two distinct states would emit an edge
// on every tick of it.
func isUp(status string) bool { return status == registry.HealthUp }

// DetectTransitions compares each fresh result against the last one
// recorded for the same (stack, region) and returns the edges.
//
// MUST be called BEFORE the results are persisted — once `PutHealth`
// has run, `LatestHealth` returns the new result and every edge has
// been erased. That ordering is the entire contract of this function
// and is why it is not folded into the persist loop.
//
// A (stack, region) with NO prior result produces no transition. This
// is load-bearing: the first sweep after the component is deployed
// fresh would otherwise emit an edge for every service in the fleet at
// once, which is both wrong (nothing changed; we just started looking)
// and a storm on its first minute of life.
func DetectTransitions(ctx context.Context, store registry.Store, results []registry.HealthResult) []Transition {
	// One LatestHealth read per STACK, not per result: a multi-region
	// service produces one result per region and the store returns the
	// whole region map in one call.
	priors := map[string]map[string]registry.HealthResult{}

	var out []Transition
	for _, res := range results {
		prior, cached := priors[res.Stack]
		if !cached {
			// A read failure is not an edge. Reporting "down" because
			// storage hiccuped would page the operator about the
			// observatory, which has its own health path; swallowing it
			// at worst delays one real notification to the next tick.
			p, err := store.LatestHealth(ctx, res.Stack)
			if err != nil {
				slog.Warn("edge detection: latest health unavailable", "stack", res.Stack, "err", err)
				p = nil
			}
			prior = p
			priors[res.Stack] = p
		}
		prev, found := prior[res.Region]
		if !found || prev.Status == "" {
			continue
		}
		if isUp(prev.Status) == isUp(res.Status) {
			continue
		}
		out = append(out, Transition{
			Stack:  res.Stack,
			Region: res.Region,
			From:   prev.Status,
			To:     res.Status,
			At:     res.At,
			Result: res,
		})
	}
	return out
}

// PublishTransitions emits one event per edge.
//
// Inline and sequential on the caller's goroutine. No `go func()`: a
// Lambda freezes background goroutines the instant the handler returns,
// so a notification deferred to one silently never happens — and this
// code path exists precisely to stop notifications from silently never
// happening.
//
// Errors are logged, never returned. A sweep's job is to produce a
// health picture and persist it; a notifier being slow must not make
// the tick fail, because a failed tick is a sweep that did not run.
func PublishTransitions(ctx context.Context, pub notify.Publisher, transitions []Transition) {
	if pub == nil || !pub.Enabled() {
		return
	}
	for _, t := range transitions {
		if err := pub.Publish(ctx, transitionEvent(pub, t)); err != nil {
			slog.Error("publish health transition", "stack", t.Stack, "region", t.Region,
				"from", t.From, "to", t.To, "err", err)
		}
	}
}

// transitionEvent renders one edge as a notifier event.
func transitionEvent(pub notify.Publisher, t Transition) notify.Event {
	subject := t.Stack
	if t.Region != "" {
		subject += "/" + t.Region
	}

	severity := notify.SeverityError
	title := fmt.Sprintf("Service down: %s", subject)
	if t.Recovered() {
		// Warning, not info. Nothing is wrong any more, so it is not an
		// error — but an operator who was paged about the outage has to
		// see it end, and `info` sits below every channel's default
		// minimum, which is exactly how forge-notifier lost every
		// `info` notification it ever sent.
		severity = notify.SeverityWarning
		title = fmt.Sprintf("Service recovered: %s", subject)
	}

	return notify.Event{
		ID:       notify.HealthEdgeEventID(t.Stack, t.Region, t.To, t.At),
		Source:   t.Stack,
		Kind:     notify.KindHealthChanged,
		Severity: severity,
		Subject:  subject,
		Title:    title,
		Body:     transitionBody(t),
		URL:      pub.RunLink("/#/services/" + t.Stack),
		At:       t.At,
		Labels: map[string]string{
			"stack":  t.Stack,
			"region": t.Region,
			"from":   t.From,
			"to":     t.To,
		},
	}
}

// transitionBody renders the diagnosis an operator reads in Telegram,
// where they have no other view of it.
func transitionBody(t Transition) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s → %s\n", t.From, t.To)
	if t.Result.HTTPStatus > 0 {
		fmt.Fprintf(&b, "http: %d\n", t.Result.HTTPStatus)
	}
	if t.Result.LatencyMS > 0 {
		fmt.Fprintf(&b, "latency: %dms\n", t.Result.LatencyMS)
	}
	if t.Result.Error != "" {
		// A transport failure and an HTTP 500 are different faults and
		// the message must not collapse them: HTTPStatus is zero for the
		// first, and this line is the only evidence of what happened.
		fmt.Fprintf(&b, "error: %s\n", t.Result.Error)
	}
	if t.Result.Version != "" {
		fmt.Fprintf(&b, "version: %s\n", t.Result.Version)
	}
	return b.String()
}
