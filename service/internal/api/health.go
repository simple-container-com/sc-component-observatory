// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/health"
	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// HealthConfig wires the probe sweep and its read endpoints.
type HealthConfig struct {
	Registry registry.Store
	Prober   health.Prober

	// DefaultPath is appended to a service endpoint when the service
	// declares no health path of its own.
	DefaultPath string

	// RetainDays caps stored results; the sweep prunes past it.
	RetainDays int

	// Now is injected for tests.
	Now func() time.Time
}

func (c *HealthConfig) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

// Tick serves `POST /api/v1/scheduler/healthcheck/tick`.
//
// Fired by whatever scheduling primitive the deploy template offers —
// an EventBridge rule on Lambda, a cron elsewhere. The handler is
// stateless and idempotent, so a missed tick only delays the next
// health reading by one interval and a double tick costs two probes.
//
// Runs the sweep INLINE rather than in a goroutine. On Lambda a
// background goroutine is frozen the moment the handler returns, so
// "fire and forget" silently does nothing — the trap that cost
// sc-component-backup a whole debugging session. A fleet-sized sweep is
// bounded by concurrency and the per-probe timeout, so it fits a normal
// request.
func (c *HealthConfig) Tick(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	prober := c.Prober
	if prober == nil {
		prober = health.NewHTTPProber(5 * time.Second)
	}

	results, err := health.Sweep(r.Context(), c.Registry, prober, health.SweepConfig{
		DefaultPath: c.DefaultPath,
		Now:         c.now,
	})
	if err != nil {
		slog.Error("healthcheck sweep failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "sweep failed: "+err.Error())
		return
	}

	// Prune on the same tick. Keeps retention from needing its own
	// schedule, and the cost is one LIST on a prefix we already touch.
	pruned := 0
	if c.RetainDays > 0 {
		if n, perr := c.Registry.PruneHealth(r.Context(), c.now().AddDate(0, 0, -c.RetainDays)); perr != nil {
			slog.Warn("healthcheck prune failed", "err", perr)
		} else {
			pruned = n
		}
	}

	var up, down, unknown int
	for _, res := range results {
		switch res.Status {
		case registry.HealthUp:
			up++
		case registry.HealthDown:
			down++
		default:
			unknown++
		}
	}
	slog.Info("healthcheck sweep", "probed", len(results), "up", up, "down", down, "unknown", unknown, "pruned", pruned)

	writeJSON(w, http.StatusOK, map[string]any{
		"probed":  len(results),
		"up":      up,
		"down":    down,
		"unknown": unknown,
		"pruned":  pruned,
		"results": results,
	})
}

// History serves `GET /api/v1/services/{stack}/health` — the probe
// timeline for one service.
func (c *HealthConfig) History(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	stack := pathParam(r, "stack")
	if stack == "" {
		writeErr(w, http.StatusBadRequest, "stack path param required")
		return
	}
	if _, found, err := c.Registry.Get(r.Context(), stack); err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	} else if !found {
		writeErr(w, http.StatusNotFound, "service not registered")
		return
	}

	hist, err := c.Registry.HealthHistory(r.Context(), stack, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "health history failed")
		return
	}
	latest, err := c.Registry.LatestHealth(r.Context(), stack)
	if err != nil {
		latest = map[string]registry.HealthResult{}
	}

	// Always emit arrays/objects, never null — a null crashes a JS
	// spread, which is how the first SC Component's SPA died.
	out := make([]registry.HealthResult, 0, len(hist))
	out = append(out, hist...)
	byRegion := map[string]registry.HealthResult{}
	for k, v := range latest {
		byRegion[k] = v
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"stack":   stack,
		"latest":  byRegion,
		"history": out,
	})
}

// summarizeHealth reduces per-region results to one status for a list
// row.
//
// Worst-wins: if any region is down the service shows down. A service
// that is up in one region and down in another is NOT healthy, and
// averaging or majority-voting would hide a real partial outage behind
// a green tick.
func summarizeHealth(latest map[string]registry.HealthResult) (status string, at time.Time) {
	if len(latest) == 0 {
		return registry.HealthUnknown, time.Time{}
	}
	status = registry.HealthUp
	sawKnown := false
	for _, res := range latest {
		if res.At.After(at) {
			at = res.At
		}
		switch res.Status {
		case registry.HealthDown:
			status = registry.HealthDown
			sawKnown = true
		case registry.HealthUp:
			sawKnown = true
		}
	}
	if !sawKnown {
		return registry.HealthUnknown, at
	}
	return status, at
}
