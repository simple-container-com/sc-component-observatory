// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// tickReq builds an authorized scheduler tick request.
func tickReq() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/scheduler/healthcheck/tick", nil)

	return req.WithContext(context.WithValue(req.Context(), claimsKey{}, &Claims{Op: OpScheduler}))
}

// TestTick_DrivesReplicationTick pins the whole point of the wiring: the
// healthcheck tick is the only thing in this service that runs on a real
// invocation which stays awake until its work finishes, so it is the only
// place go-embedb's five background loops can be driven from.
//
// Asserting "called once" is not enough — the call must also be bounded,
// because a snapshot plus a GC sweep on a cold bucket can outlast the
// Lambda timeout and would then starve the healthcheck sweep that is this
// endpoint's actual job.
func TestTick_DrivesReplicationTick(t *testing.T) {
	calls := 0
	hadDeadline := false

	cfg := &HealthConfig{
		Registry: registry.NewInMemoryStore(),
		ReplicationTick: func(ctx context.Context) error {
			calls++
			_, hadDeadline = ctx.Deadline()

			return nil
		},
	}

	rec := httptest.NewRecorder()
	cfg.Tick(rec, tickReq())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("replication tick called %d times, want exactly 1", calls)
	}
	if !hadDeadline {
		t.Error("replication tick ran on a context with no deadline; it must be bounded so it cannot consume the whole invocation")
	}
}

// TestTick_ReplicationFailureIsNotFatal: a replication error is retried on
// the next tick, whereas failing the tick would stop the ENTIRE fleet's
// healthchecks over one transient S3 error. The sweep must still run.
func TestTick_ReplicationFailureIsNotFatal(t *testing.T) {
	cfg := &HealthConfig{
		Registry: registry.NewInMemoryStore(),
		ReplicationTick: func(context.Context) error {
			return errors.New("put body: operation error S3: PutObject, StatusCode: 0")
		},
	}

	rec := httptest.NewRecorder()
	cfg.Tick(rec, tickReq())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a replication failure must not fail the sweep (body %s)", rec.Code, rec.Body.String())
	}
}

// TestTick_ReplicationRunsEvenWithoutARegistry is deliberate, and the
// reason the call sits ABOVE the registry check: a half-configured stack
// must still renew its slot heartbeat and leases. Otherwise a component
// whose registry wiring is missing silently stops replicating too, and
// the slot expires while the Lambda is serving traffic — the exact shape
// measured across the fleet on 2026-11-25 (`heartbeat == joinedAt`).
func TestTick_ReplicationRunsEvenWithoutARegistry(t *testing.T) {
	calls := 0

	cfg := &HealthConfig{
		// No Registry — the handler will 501.
		ReplicationTick: func(context.Context) error {
			calls++

			return nil
		},
	}

	rec := httptest.NewRecorder()
	cfg.Tick(rec, tickReq())

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 with no registry (body %s)", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("replication tick called %d times, want 1 — it must run before the registry check", calls)
	}
}

// TestTick_NilReplicationTickIsSkipped: the field is nil on any deploy not
// using the embed (S3/filesystem storage), and that must stay a no-op
// rather than a nil-call panic.
func TestTick_NilReplicationTickIsSkipped(t *testing.T) {
	cfg := &HealthConfig{Registry: registry.NewInMemoryStore()}

	rec := httptest.NewRecorder()
	cfg.Tick(rec, tickReq())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestHistory_DoesNotDriveReplication guards the mistake this wiring made
// on its first pass: a pattern-based edit inserted the same block into
// History, a plain GET read endpoint. That would drive a snapshot + GC
// sweep on every SPA page load — unbounded fan-out of S3 writes from a
// read path, and a latency cost on the operator UI.
func TestHistory_DoesNotDriveReplication(t *testing.T) {
	calls := 0

	cfg := &HealthConfig{
		Registry: registry.NewInMemoryStore(),
		ReplicationTick: func(context.Context) error {
			calls++

			return nil
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/services/some-stack/health", nil)
	rec := httptest.NewRecorder()
	cfg.History(rec, req)

	if calls != 0 {
		t.Errorf("replication tick called %d times from the read path, want 0", calls)
	}
}
