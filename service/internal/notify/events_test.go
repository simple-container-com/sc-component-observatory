// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests are the sibling of sc-component-backup's
// internal/backup/notify/events_test.go, for the same reason the
// publisher itself is a second copy: there is no shared module for the
// two to live in. Keep them diffable.

// No NOTIFY_URL means notifications are off, and that is a legitimate
// configuration — every existing stack must keep booting on the release
// that adds this.
func TestNewEmitterDisabledWhenNoURL(t *testing.T) {
	pub, err := NewEmitter(EmitterConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.Enabled() {
		t.Error("a publisher with no URL must not report itself enabled")
	}
	if err := pub.Publish(context.Background(), Event{Kind: KindHealthChanged}); err != nil {
		t.Errorf("the disabled publisher must swallow a publish: %v", err)
	}
	if link := pub.RunLink("/#/services/x"); link != "" {
		t.Errorf("RunLink on a disabled publisher = %q, want empty", link)
	}
}

// The half-wired cases. Each one would otherwise surface for the first
// time at the moment an operator was relying on being paged, so each is
// a STARTUP failure — and the message has to name the key to fix,
// because "notifications don't work" is otherwise a four-layer debug
// across plugin, env, token and channel.
func TestNewEmitterFailsLoudlyOnHalfWiring(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   EmitterConfig
		wants []string
	}{
		{
			// SC leaves a placeholder it did not resolve in place
			// verbatim. Without this check the service starts happily
			// and POSTs every event to a host beginning with a dollar
			// sign — live-proven on THIS component's healthcheck tick,
			// 2026-11-19, where the EventBridge rule's request payload
			// kept the literal and the tick answered 401 twice.
			name:  "unresolved tpl-extension placeholder",
			cfg:   EmitterConfig{BaseURL: "$sc:${resource:my-notifier.self-url}", Token: "t"},
			wants: []string{"unresolved SC placeholder", "notify-url", "deployTo"},
		},
		{
			name:  "unresolved secret reference",
			cfg:   EmitterConfig{BaseURL: "${secret:notifier-url}", Token: "t"},
			wants: []string{"unresolved SC placeholder"},
		},
		{
			name:  "url without a token",
			cfg:   EmitterConfig{BaseURL: "https://notify.simple-forge.com"},
			wants: []string{"NOTIFY_TOKEN", "401"},
		},
		{
			name:  "token without a url",
			cfg:   EmitterConfig{Token: "t"},
			wants: []string{"NOTIFY_TOKEN", "NOTIFY_URL", "half-wired"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub, err := NewEmitter(tc.cfg)
			if err == nil {
				t.Fatalf("expected an error, got publisher enabled=%v", pub.Enabled())
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message does not mention %q:\n  %v", want, err)
				}
			}
		})
	}
}

// One POST, exactly once, with the auth header and the UA. The
// single-attempt assertion is the regression: retry/backoff/give-up
// live only in the notifier's delivery queue, and two layers each
// owning retry is what sent 20+ duplicate Telegram messages on
// 2026-09-19.
func TestPublishSendsOncePerEvent(t *testing.T) {
	var calls int32
	var got Event
	var auth, ua, path string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		auth, ua, path = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	pub, err := NewEmitter(EmitterConfig{BaseURL: srv.URL + "/", Token: "tok", SelfURL: "https://monitor.example.com/"})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	if !pub.Enabled() {
		t.Fatal("a fully-wired publisher must report itself enabled")
	}

	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	err = pub.Publish(context.Background(), Event{
		ID: HealthEdgeEventID("forge-baas", "us-east-1", "down", at), Kind: KindHealthChanged,
		Severity: SeverityError, Subject: "forge-baas/us-east-1", Title: "Service down: forge-baas/us-east-1", At: at,
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1 — the publisher must not retry", n)
	}
	if path != eventsPath {
		// A trailing slash on NOTIFY_URL used to produce `//api/v1/...`
		// in the sibling components; normalising at construction is
		// what keeps this path exact.
		t.Errorf("path = %q, want %q", path, eventsPath)
	}
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if !strings.Contains(ua, "sc-component-observatory") {
		// Cloudflare answers the default Go UA with a 403 (error 1010).
		t.Errorf("User-Agent = %q, want a named client", ua)
	}
	if got.ID != "health-edge:forge-baas:us-east-1:down:2026-11-23T10:00:00Z" {
		t.Errorf("event id = %q", got.ID)
	}
	if !got.At.Equal(at) {
		t.Errorf("At = %v, want %v", got.At, at)
	}
	if link := pub.RunLink("/#/services/forge-baas"); link != "https://monitor.example.com/#/services/forge-baas" {
		t.Errorf("RunLink = %q", link)
	}
}

// An event published with no timestamp gets one, rather than reaching
// the notifier with a zero time that renders as year 1.
func TestPublishStampsMissingTimestamp(t *testing.T) {
	var got Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pub, err := NewEmitter(EmitterConfig{BaseURL: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	if err := pub.Publish(context.Background(), Event{Kind: KindHealthChanged}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.At.IsZero() {
		t.Error("At must be filled in when the caller leaves it zero")
	}
}

// A 401/403 has exactly one cause — the token was not minted by the key
// this notifier validates with — and the generic "non-2xx" message
// sends people reading network logs instead of re-minting.
func TestPublishNamesTheTokenOn401(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"invalid token"}`))
		}))
		pub, err := NewEmitter(EmitterConfig{BaseURL: srv.URL, Token: "stale"})
		if err != nil {
			t.Fatalf("NewEmitter: %v", err)
		}
		err = pub.Publish(context.Background(), Event{Kind: KindHealthChanged})
		srv.Close()

		if err == nil {
			t.Fatalf("HTTP %d must be an error", code)
		}
		for _, want := range []string{"NOTIFY_TOKEN", "signingKey", "re-mint"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("HTTP %d: message does not mention %q:\n  %v", code, want, err)
			}
		}
	}
}

// Other non-2xx codes surface with the status and a bounded prefix of
// the body. Bounded on purpose: an unbounded read on a misrouted URL
// pulls a whole SPA bundle into a log line.
func TestPublishReportsOtherNon2xxWithBoundedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	}))
	defer srv.Close()

	pub, err := NewEmitter(EmitterConfig{BaseURL: srv.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	err = pub.Publish(context.Background(), Event{Kind: KindHealthChanged})
	if err == nil {
		t.Fatal("HTTP 500 must be an error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("message should name the status: %v", err)
	}
	if n := strings.Count(err.Error(), "x"); n > 600 {
		t.Errorf("error body prefix is %d chars — should be bounded to ~512", n)
	}
}

// The idempotency key is derived from the edge that happened, never
// from the clock the publisher read: a re-fired scheduler tick and two
// overlapping sweeps must produce the same ID so the notifier dedupes
// rather than paging twice about one outage.
//
// But the probe's own timestamp IS in the key, deliberately. A service
// that goes down, recovers and goes down again inside the dedupe window
// is three real events, and rounding the time away would collapse the
// second outage into the first.
func TestHealthEdgeEventIDIsStableAndDiscriminating(t *testing.T) {
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	a := HealthEdgeEventID("forge-baas", "us-east-1", "down", at)

	if b := HealthEdgeEventID("forge-baas", "us-east-1", "down", at); a != b {
		t.Errorf("not stable across calls: %q vs %q", a, b)
	}
	// Same wall-clock instant expressed in another zone is the same
	// edge — the key normalises to UTC so a prober in a non-UTC zone
	// cannot double-page.
	moscow := time.FixedZone("MSK", 3*60*60)
	if c := HealthEdgeEventID("forge-baas", "us-east-1", "down", at.In(moscow)); a != c {
		t.Errorf("timezone changed the key: %q vs %q", a, c)
	}

	for _, tc := range []struct {
		name string
		id   string
	}{
		{"another stack", HealthEdgeEventID("forge-storage", "us-east-1", "down", at)},
		{"another region", HealthEdgeEventID("forge-baas", "eu-west-1", "down", at)},
		{"the recovery edge", HealthEdgeEventID("forge-baas", "us-east-1", "up", at)},
		{"a later outage", HealthEdgeEventID("forge-baas", "us-east-1", "down", at.Add(time.Minute))},
	} {
		if tc.id == a {
			t.Errorf("%s must not collide with the first edge: %q", tc.name, tc.id)
		}
	}
}
