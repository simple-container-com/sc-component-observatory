// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package api is observatory's HTTP surface.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// ServicesConfig wires the registry handlers.
type ServicesConfig struct {
	Registry registry.Store

	// Now is injected for tests; defaults to time.Now.
	Now func() time.Time
}

func (c *ServicesConfig) now() time.Time {
	if c.Now == nil {
		return time.Now().UTC()
	}
	return c.Now().UTC()
}

// serviceJSON is the wire shape for one row of the dashboard.
//
// Deliberately a separate type from registry.ServiceRecord rather than
// marshalling the record directly. The first SC Component shipped six
// endpoints its SPA called and one array field its SPA spread, and a
// single absent field produced `[...undefined]` → "e is not iterable",
// a blank screen with no clue. An explicit wire type means every field
// the UI reads is visible in one place and can be asserted in a test.
//
// Rules that follow from that: slices are ALWAYS present (never null),
// and anything the UI needs computed is computed here rather than in
// the browser.
type serviceJSON struct {
	Stack       string `json:"stack"`
	Service     string `json:"service"`
	Env         string `json:"env"`
	Cloud       string `json:"cloud"`
	Template    string `json:"template"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	ShortCommit string `json:"short_commit"`
	DeployedAt  string `json:"deployed_at"`
	FirstSeenAt string `json:"first_seen_at"`
	LastSeenAt  string `json:"last_seen_at"`
	EndpointURL string `json:"endpoint_url"`

	// Sources is never null — an empty array renders as "no telemetry
	// wired" rather than crashing a `.map()`.
	Sources []sourceJSON `json:"sources"`

	// Inferred tells the UI to show a warning badge: these handles were
	// guessed from naming conventions, not published by the template
	// plugin that owns the resource.
	Inferred bool `json:"inferred"`

	// Stale is computed server-side so the UI has no clock logic.
	Stale bool `json:"stale"`

	// TelemetryReady is false when we have no way to query this
	// service's logs or metrics. Surfaced explicitly because an empty
	// chart that looks like "no traffic" is worse than one that says
	// "not wired up" — the first Component taught us that silent
	// degradation costs more than a blunt error.
	TelemetryReady bool `json:"telemetry_ready"`

	// Health is the worst-wins summary across regions, one of
	// up/down/unknown, with the timestamp of the newest probe behind
	// it. Computed server-side so the UI holds no health logic.
	Health     string `json:"health"`
	HealthAt   string `json:"health_at"`
	HealthPath string `json:"health_path"`

	// Regions is never null.
	Regions []string `json:"regions"`

	// HasClientConfig tells the UI whether a detail view can render the
	// deploy config, without shipping the whole map in the list
	// response — the list is the hot path and a compose block is large.
	HasClientConfig bool `json:"has_client_config"`
}

type sourceJSON struct {
	Kind   string            `json:"kind"`
	Ref    string            `json:"ref"`
	Region string            `json:"region"`
	Dims   map[string]string `json:"dims"`
}

func toServiceJSON(r registry.ServiceRecord, now time.Time) serviceJSON {
	out := serviceJSON{
		Stack:       r.Stack,
		Service:     r.Service,
		Env:         r.Env,
		Cloud:       r.Cloud,
		Template:    r.Template,
		Version:     r.Version,
		Commit:      r.Commit,
		ShortCommit: r.ShortCommit(),
		EndpointURL: r.EndpointURL,
		Sources:     make([]sourceJSON, 0, len(r.Sources)),
		Inferred:    r.Inferred,
		Stale:       r.IsStale(now),
	}
	if !r.DeployedAt.IsZero() {
		out.DeployedAt = r.DeployedAt.UTC().Format(time.RFC3339)
	}
	if !r.FirstSeenAt.IsZero() {
		out.FirstSeenAt = r.FirstSeenAt.UTC().Format(time.RFC3339)
	}
	if !r.LastSeenAt.IsZero() {
		out.LastSeenAt = r.LastSeenAt.UTC().Format(time.RFC3339)
	}
	for _, s := range r.Sources {
		dims := s.Dims
		if dims == nil {
			dims = map[string]string{}
		}
		out.Sources = append(out.Sources, sourceJSON{
			Kind: s.Kind, Ref: s.Ref, Region: s.Region, Dims: dims,
		})
	}
	out.TelemetryReady = len(out.Sources) > 0
	out.HealthPath = r.HealthPath
	out.Regions = make([]string, 0, len(r.Regions))
	out.Regions = append(out.Regions, r.Regions...)
	out.HasClientConfig = len(r.ClientConfig) > 0
	// Health defaults to unknown; List/Get overlay the real summary.
	out.Health = registry.HealthUnknown
	return out
}

// List serves `GET /api/v1/services` — the dashboard's main table.
func (c *ServicesConfig) List(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	recs, err := c.Registry.List(r.Context())
	if err != nil {
		slog.Error("services list failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "list failed")
		return
	}
	now := c.now()
	out := make([]serviceJSON, 0, len(recs))
	for _, rec := range recs {
		row := toServiceJSON(rec, now)
		// Health is read per service rather than in one bulk query
		// because the colocated store has no cross-service index. Bounded
		// by fleet size (tens), and LatestHealth reads one capped page.
		if latest, err := c.Registry.LatestHealth(r.Context(), rec.Stack); err == nil {
			status, at := summarizeHealth(latest)
			row.Health = status
			if !at.IsZero() {
				row.HealthAt = at.UTC().Format(time.RFC3339)
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":    len(out),
		"services": out,
	})
}

// Get serves `GET /api/v1/services/{stack}` — one service plus its
// deploy timeline.
func (c *ServicesConfig) Get(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	stack := pathParam(r, "stack")
	if stack == "" {
		writeErr(w, http.StatusBadRequest, "stack path param required")
		return
	}
	rec, found, err := c.Registry.Get(r.Context(), stack)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "service not registered")
		return
	}
	hist, err := c.Registry.DeployHistory(r.Context(), stack, 50)
	if err != nil {
		// The timeline is additive — a history read failure must not
		// hide the service itself.
		slog.Warn("deploy history read failed", "stack", stack, "err", err)
		hist = nil
	}
	events := make([]registry.DeployEvent, 0, len(hist))
	events = append(events, hist...)

	svc := toServiceJSON(rec, c.now())
	latestHealth := map[string]registry.HealthResult{}
	if latest, herr := c.Registry.LatestHealth(r.Context(), stack); herr == nil {
		latestHealth = latest
		status, at := summarizeHealth(latest)
		svc.Health = status
		if !at.IsZero() {
			svc.HealthAt = at.UTC().Format(time.RFC3339)
		}
	}

	// client_config is served only on the detail view. Already stripped
	// of secrets plugin-side before it was ever stored; emitted as an
	// object (never null) so the UI can iterate it unguarded.
	clientConfig := rec.ClientConfig
	if clientConfig == nil {
		clientConfig = map[string]any{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service":       svc,
		"deploys":       events,
		"health":        latestHealth,
		"client_config": clientConfig,
	})
}

// Delete serves `DELETE /api/v1/services/{stack}`.
//
// Ships in slice 1 on purpose. The plugin bus has no "consumer
// destroyed" event — PreDestroy fires only for the component's own
// resource — so without this an operator has no clean way to retire a
// service. The first SC Component shipped this late and spent a session
// scrubbing an orphan by hand; a dashboard listing dead services is
// actively misleading, which makes it worse here than it was there.
func (c *ServicesConfig) Delete(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	stack := pathParam(r, "stack")
	if stack == "" {
		writeErr(w, http.StatusBadRequest, "stack path param required")
		return
	}
	// 404 before deleting so a typo is visible rather than a silent
	// no-op that looks like success.
	if _, found, err := c.Registry.Get(r.Context(), stack); err != nil {
		writeErr(w, http.StatusInternalServerError, "lookup failed")
		return
	} else if !found {
		writeErr(w, http.StatusNotFound, "service not registered")
		return
	}
	if err := c.Registry.Delete(r.Context(), stack); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": stack})
}

// registerRequest is the plugin's push-register body.
type registerRequest struct {
	Stack       string   `json:"stack"`
	Cloud       string   `json:"cloud"`
	Template    string   `json:"template"`
	Version     string   `json:"version"`
	Commit      string   `json:"commit"`
	EndpointURL string   `json:"endpoint_url"`
	Inferred    bool     `json:"inferred"`
	Regions     []string `json:"regions"`
	HealthPath  string   `json:"health_path"`

	// ClientConfig is the consumer's full resolved client config, minus
	// secrets. Stored verbatim so a detail view can show what was
	// actually deployed without the component guessing in advance which
	// fields an operator will want.
	ClientConfig map[string]any `json:"client_config"`

	Sources []struct {
		Kind   string            `json:"kind"`
		Ref    string            `json:"ref"`
		Region string            `json:"region"`
		Dims   map[string]string `json:"dims"`
	} `json:"sources"`
}

// Register serves `POST /api/v1/services/register`.
//
// Called by the plugin's ComputeProcess on every consumer deploy. Auth
// is the same HS256 JWT shape the first Component uses: the token's
// `op` must be "register" and its `tenant` claim must equal the body's
// stack, so one consumer cannot register under another's name.
func (c *ServicesConfig) Register(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	claims := ClaimsFromContext(r.Context())
	if claims == nil || claims.Op != "register" {
		writeErr(w, http.StatusForbidden, "token op must be 'register'")
		return
	}

	var body registerRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return
	}
	body.Stack = strings.TrimSpace(body.Stack)
	if body.Stack == "" {
		writeErr(w, http.StatusBadRequest, "stack is required")
		return
	}
	if claims.Tenant != body.Stack {
		writeErr(w, http.StatusForbidden, "JWT tenant does not match body stack")
		return
	}

	rec := registry.ServiceRecord{
		Stack:        body.Stack,
		Cloud:        body.Cloud,
		Template:     body.Template,
		Version:      body.Version,
		Commit:       body.Commit,
		EndpointURL:  body.EndpointURL,
		Inferred:     body.Inferred,
		Regions:      body.Regions,
		HealthPath:   body.HealthPath,
		ClientConfig: body.ClientConfig,
		DeployedAt:   c.now(),
	}
	for _, s := range body.Sources {
		rec.Sources = append(rec.Sources, registry.TelemetrySource{
			Kind: s.Kind, Ref: s.Ref, Region: s.Region, Dims: s.Dims,
		})
	}

	saved, err := registry.Register(r.Context(), c.Registry, rec, c.now())
	if err != nil {
		slog.Error("service register failed", "stack", rec.Stack, "err", err)
		writeErr(w, http.StatusInternalServerError, "register failed")
		return
	}

	slog.Info(
		"service registered",
		"stack", saved.Stack, "version", saved.Version,
		"cloud", saved.Cloud, "template", saved.Template,
		"sources", len(saved.Sources), "inferred", saved.Inferred,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": toServiceJSON(saved, c.now()),
	})
}
