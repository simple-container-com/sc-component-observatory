// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// TestProbe_ColdStartIsNotDown is the regression guard for the reading
// that lands at the worst moment.
//
// A container-image Lambda that has just been deployed can take several
// seconds to answer its first request. With no retry that is recorded as
// `down, latency_ms: 5000` — seconds after a SUCCESSFUL deploy, which is
// exactly when an operator is looking at the dashboard. Live-caught
// 2026-11-19 on the first tenant, where curl against the same URL
// answered 200 in 0.56s and the next sweep read 58ms.
//
// The server below fails the first request at the transport level (by
// hijacking and closing the connection, which is what a timeout looks
// like to the client) and answers the second.
func TestProbe_ColdStartIsNotDown(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			// Kill the connection: a transport-level failure, not a status.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","version":"2026.11.19-abc1234"}`))
	}))
	defer srv.Close()

	res := NewHTTPProber(2*time.Second).Probe(context.Background(), srv.URL+"/healthz")

	if res.Status != registry.HealthUp {
		t.Fatalf("status = %q (err %q), want up — a cold start must not read as down", res.Status, res.Error)
	}
	if !res.Retried {
		t.Error("Retried = false; the result should record that the first attempt failed")
	}
	if res.Version != "2026.11.19-abc1234" {
		t.Errorf("Version = %q, want the probe-reported version", res.Version)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d, want exactly 2 (one retry, not a loop)", got)
	}
}

// TestProbe_HTTPStatusIsNotRetried — the service ANSWERED. A 500 twice
// is the same 500, and retrying every error response would double the
// request volume against a service that is already struggling.
func TestProbe_HTTPStatusIsNotRetried(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	res := NewHTTPProber(2*time.Second).Probe(context.Background(), srv.URL+"/healthz")

	if res.Status != registry.HealthDown {
		t.Errorf("status = %q, want down", res.Status)
	}
	if res.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("HTTPStatus = %d, want 500", res.HTTPStatus)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("hits = %d, want 1 — an HTTP status must not be retried", got)
	}
	if res.Retried {
		t.Error("Retried = true on an HTTP status")
	}
}

// TestProbe_401IsDown — a health path behind auth is a real
// misconfiguration, and counting it as up would mask a broken deploy
// behind a green tick. This is why forge-storage needed a public
// /healthz: every unauthenticated path answered 401.
func TestProbe_401IsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	res := NewHTTPProber(time.Second).Probe(context.Background(), srv.URL+"/healthz")
	if res.Status != registry.HealthDown {
		t.Errorf("status = %q, want down for a 401", res.Status)
	}
}

// TestProbe_VersionBodyIsBestEffort — a 200 with a body that is not the
// expected JSON is still HEALTHY. That is what the status code said, and
// downgrading a service because its body did not match a convention it
// never agreed to would be a false outage.
func TestProbe_VersionBodyIsBestEffort(t *testing.T) {
	for name, body := range map[string]string{
		"html":       "<html><body>ok</body></html>",
		"empty":      "",
		"json-array": `[1,2,3]`,
		"other-keys": `{"ok":true}`,
		"non-string": `{"version":42}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		res := NewHTTPProber(time.Second).Probe(context.Background(), srv.URL+"/healthz")
		if res.Status != registry.HealthUp {
			t.Errorf("%s: status = %q, want up", name, res.Status)
		}
		if res.Version != "" {
			t.Errorf("%s: Version = %q, want empty (a coerced value reads like a real version)", name, res.Version)
		}
		srv.Close()
	}
}

func TestJoinHealthURL(t *testing.T) {
	// A Function URL always has a trailing slash; an ingress hostname
	// usually does not. Neither may double or drop the separator.
	cases := map[[2]string]string{
		{"https://x.lambda-url.aws/", "/healthz"}: "https://x.lambda-url.aws/healthz",
		{"https://a.example.com", "/healthz"}:     "https://a.example.com/healthz",
		{"https://a.example.com", "healthz"}:      "https://a.example.com/healthz",
		{"https://a.example.com/", ""}:            "https://a.example.com",
	}
	for in, want := range cases {
		if got := JoinHealthURL(in[0], in[1]); got != want {
			t.Errorf("JoinHealthURL(%q, %q) = %q, want %q", in[0], in[1], got, want)
		}
	}
}

// TestSweep_FallsBackToHealthOn404 covers the fleet's split spelling.
//
// forge-notifier, forge-storage, forge-aigateway and forge-conductor
// serve /healthz; forge-concierge and forge-sessions serve /health.
// Neither is wrong, and there is nowhere to record the difference — SC's
// client config has no healthPath field, so a value written there is
// dropped at parse and never reaches the descriptor this component
// reads.
//
// So a 404 on a DEFAULT path gets one more try. Without this, half the
// fleet would show permanently DOWN while being perfectly healthy —
// which is the single most damaging thing a monitoring dashboard can
// do, because it trains the operator to ignore red.
func TestSweep_FallsBackToHealthOn404(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	store := registry.NewInMemoryStore()
	if _, err := registry.Register(context.Background(), store, registry.ServiceRecord{
		Stack:       "forge-sessions--staging",
		EndpointURL: srv.URL,
	}, time.Now().UTC()); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := Sweep(context.Background(), store, NewHTTPProber(2*time.Second), SweepConfig{DefaultPath: "/healthz"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Results) != 1 || res.Results[0].Status != registry.HealthUp {
		t.Fatalf("status = %+v, want up via the /health fallback", res.Results)
	}
	if len(paths) != 2 || paths[0] != "/healthz" || paths[1] != "/health" {
		t.Errorf("paths probed = %v, want [/healthz /health]", paths)
	}
}

// TestSweep_DeclaredPathIsNotSecondGuessed — a path the service
// DECLARED is authoritative. If an operator says the probe lives at
// /ping, a 404 there is a real failure, and quietly succeeding on some
// other path would hide a broken deploy behind a green tick.
func TestSweep_DeclaredPathIsNotSecondGuessed(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	store := registry.NewInMemoryStore()
	if _, err := registry.Register(context.Background(), store, registry.ServiceRecord{
		Stack:       "declared--staging",
		EndpointURL: srv.URL,
		HealthPath:  "/ping",
	}, time.Now().UTC()); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := Sweep(context.Background(), store, NewHTTPProber(2*time.Second), SweepConfig{DefaultPath: "/healthz"})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Results[0].Status != registry.HealthDown {
		t.Errorf("status = %q, want down — a declared path must not be second-guessed", res.Results[0].Status)
	}
	if len(paths) != 1 || paths[0] != "/ping" {
		t.Errorf("paths probed = %v, want only [/ping]", paths)
	}
}
