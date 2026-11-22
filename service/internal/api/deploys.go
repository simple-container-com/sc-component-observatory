// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/configdiff"
	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// deployJSON is one timeline row.
//
// An explicit wire type, like serviceJSON, and for the same reason —
// but there is a second reason here: the stored event is accompanied by
// a full client-config SNAPSHOT, and that must never be marshalled into
// a timeline. Emitting the record directly is how a 20-row page becomes
// megabytes of compose blocks against a Lambda response cap.
type deployJSON struct {
	ID              string `json:"id"`
	Stack           string `json:"stack"`
	Version         string `json:"version"`
	Commit          string `json:"commit,omitempty"`
	ShortCommit     string `json:"short_commit,omitempty"`
	Template        string `json:"template"`
	Cloud           string `json:"cloud"`
	Repository      string `json:"repository,omitempty"`
	DeployedAt      string `json:"deployed_at"`
	PreviousVersion string `json:"previous_version,omitempty"`

	// ConfigCaptured says a snapshot exists for this deploy. Surfaced
	// to the UI so "we have no snapshot" is visibly different from "the
	// config did not change" — the distinction the diff's own
	// `unavailable` field also carries, kept on the row because a
	// timeline can be rendered without diffs.
	ConfigCaptured bool `json:"config_captured"`

	// Diff is this deploy against the one before it. Nil when the
	// caller asked for a timeline without diffs.
	Diff *configdiff.Diff `json:"diff,omitempty"`
}

func toDeployJSON(ev registry.DeployEvent) deployJSON {
	out := deployJSON{
		ID:              ev.ID,
		Stack:           ev.Stack,
		Version:         ev.Version,
		Commit:          ev.Commit,
		Template:        ev.Template,
		Cloud:           ev.Cloud,
		Repository:      ev.Repository,
		PreviousVersion: ev.PreviousVersion,
		ConfigCaptured:  ev.ConfigCaptured,
	}
	// Reuse the record's own rule so a short commit is derived
	// identically everywhere, including the CalVer fallback.
	out.ShortCommit = registry.ServiceRecord{Commit: ev.Commit, Version: ev.Version}.ShortCommit()
	if !ev.DeployedAt.IsZero() {
		out.DeployedAt = ev.DeployedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// Deploy page bounds. The default is small because each row with a diff
// costs one extra snapshot GET, and a timeline is read, not scrolled.
const (
	defaultDeployPage = 20
	maxDeployPage     = 100
)

// Deploys serves `GET /api/v1/services/{stack}/deploys` — the deploy
// timeline with, for each row, what changed since the deploy before it.
//
// The diff is computed HERE from stored snapshots rather than baked at
// registration. That is what lets the whole timeline re-render under a
// better differ later; the AWS-only ancestor stored only its computed
// `previous_*` fields and could never revisit a past deploy.
func (c *ServicesConfig) Deploys(w http.ResponseWriter, r *http.Request) {
	if c.Registry == nil {
		writeErr(w, http.StatusNotImplemented, "registry not wired")
		return
	}
	// Operator-only: a diff is a rendering of another tenant's config.
	if !requireOperator(w, r) {
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

	limit := defaultDeployPage
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, maxDeployPage)
	}

	// One event beyond the page, used ONLY as the oldest row's diff
	// baseline. Without it the last row on every page would report
	// "nothing to compare against" — technically true of the page, and
	// a lie about the service.
	hist, err := c.Registry.DeployHistory(r.Context(), stack, limit+1)
	if err != nil {
		slog.Error("deploy history read failed", "stack", stack, "err", err)
		writeErr(w, http.StatusInternalServerError, "deploy history read failed")
		return
	}

	page := hist
	hasBaseline := false
	if len(hist) > limit {
		page = hist[:limit]
		hasBaseline = true
	}

	out := make([]deployJSON, 0, len(page))
	for i, ev := range page {
		row := toDeployJSON(ev)

		// hist is newest-first, so the PREVIOUS deploy is the next index.
		var prev *registry.DeployEvent
		if i+1 < len(hist) {
			prev = &hist[i+1]
		}
		d := c.diffFor(r, stack, ev, prev, i == len(page)-1 && !hasBaseline)
		row.Diff = &d
		out = append(out, row)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"stack":   stack,
		"count":   len(out),
		"deploys": out,
	})
}

// diffFor computes one row's diff, or explains why it cannot.
//
// Every branch that returns Unavailable states a REASON. An absent
// snapshot rendering as an empty diff would read as "this deploy
// changed nothing", which is the silent-degradation failure this
// component was built to stop doing.
func (c *ServicesConfig) diffFor(
	r *http.Request, stack string,
	ev registry.DeployEvent, prev *registry.DeployEvent,
	isOldestOverall bool,
) configdiff.Diff {
	if prev == nil {
		if isOldestOverall {
			return configdiff.Unavailable("first recorded deploy — there is nothing before it to compare against")
		}
		return configdiff.Unavailable("the previous deploy is outside this page — raise `limit` to compare")
	}
	if !ev.ConfigCaptured && !prev.ConfigCaptured {
		return configdiff.Unavailable("neither this deploy nor the one before it recorded a config snapshot (both predate snapshot capture, or their plugin sent no client config)")
	}
	if !prev.ConfigCaptured {
		return configdiff.Unavailable("the previous deploy recorded no config snapshot, so there is nothing to compare against")
	}
	if !ev.ConfigCaptured {
		return configdiff.Unavailable("this deploy recorded no config snapshot")
	}

	curCfg, curFound, err := c.Registry.DeployConfig(r.Context(), stack, ev.ID)
	if err != nil || !curFound {
		if err != nil {
			slog.Warn("deploy config read failed", "stack", stack, "event", ev.ID, "err", err)
		}
		return configdiff.Unavailable("this deploy's config snapshot could not be read")
	}
	prevCfg, prevFound, err := c.Registry.DeployConfig(r.Context(), stack, prev.ID)
	if err != nil || !prevFound {
		if err != nil {
			slog.Warn("deploy config read failed", "stack", stack, "event", prev.ID, "err", err)
		}
		return configdiff.Unavailable("the previous deploy's config snapshot could not be read")
	}
	return configdiff.Compute(prevCfg, curCfg)
}
