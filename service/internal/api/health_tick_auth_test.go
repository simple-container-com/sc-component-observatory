// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// TestTick_RequiresSchedulerOrOperatorOp is the authorization gate on
// the sweep.
//
// Token VALIDITY is not enough here. Every tenant's plugin holds a
// `register` token so it can push-register itself, and the sweep makes
// an outbound HTTP request to every endpoint in the registry — so a
// tick that accepted any valid token would let any single consumer
// stack drive traffic at the whole fleet on demand.
//
// `operator` stays allowed because forcing a sweep by hand is how this
// endpoint was exercised before it had a timer, and is still the way an
// operator checks a service they just fixed.
func TestTick_RequiresSchedulerOrOperatorOp(t *testing.T) {
	cfg := &HealthConfig{Registry: registry.NewInMemoryStore()}

	for name, tc := range map[string]struct {
		claims *Claims
		want   int
	}{
		"scheduler": {claims: &Claims{Op: OpScheduler}, want: http.StatusOK},
		"operator":  {claims: &Claims{Op: OpOperator}, want: http.StatusOK},
		// A tenant's push-registration token must NOT be able to fire it.
		"register": {claims: &Claims{Op: OpRegister, Tenant: "forge-storage--staging"}, want: http.StatusForbidden},
		// An unknown op is refused rather than defaulted in.
		"unknown": {claims: &Claims{Op: "something-else"}, want: http.StatusForbidden},
		// No claims at all: the middleware would normally 401 first, but
		// the handler must not assume the middleware ran.
		"no claims": {claims: nil, want: http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/scheduler/healthcheck/tick", nil)
		if tc.claims != nil {
			req = req.WithContext(context.WithValue(req.Context(), claimsKey{}, tc.claims))
		}
		rec := httptest.NewRecorder()
		cfg.Tick(rec, req)

		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (body %s)", name, rec.Code, tc.want, rec.Body.String())
		}
	}
}
