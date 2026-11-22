// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/configdiff"
	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
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

// deployCursor is the timeline's pagination token.
//
// Reuses the telemetry cursor shape — a timestamp boundary plus the ids
// already delivered AT that boundary — because the two pagination
// problems are the same problem. Both lists are newest-first, both can
// cut through a group of entries sharing an instant, and both are served
// by a fresh container with no memory of the previous page. Giving the
// deploy timeline its own token shape would mean two "load older"
// semantics in one SPA for no gain.
//
// It is a BOUNDARY, not a pointer at an event: a cursor still resolves
// after the event it was built from has been scrubbed (DELETE sweeps a
// tenant's history), where "continue after id X" would 404 or, worse,
// silently restart at the newest page.
//
// `delivered` is everything handed out SO FAR — the whole walk, not just
// the page that ends at `ev`. That distinction is load-bearing where a
// deploy timeline differs from a log stream: deploy timestamps come from
// whatever clock the deploying CI had, so a batch of events can share
// one millisecond, and the ids to exclude at the boundary are then not
// confined to the last page. Excluding only the last page's ids makes
// page three re-serve page two — an endless "load older" that never
// advances. The registry already carries this scar in two places
// (DeployEventID's version component, and both stores' id tiebreak);
// this is the third.
func deployCursorFor(ev registry.DeployEvent, delivered []registry.DeployEvent) string {
	c := telemetry.Cursor{BeforeMS: ev.DeployedAt.UnixMilli()}
	for _, e := range delivered {
		if e.DeployedAt.UnixMilli() == c.BeforeMS {
			c.ExcludeIDs = append(c.ExcludeIDs, e.ID)
		}
	}
	return c.Encode()
}

// skipDelivered returns the index in hist where the page after cur
// starts.
//
// Everything newer than the boundary is already delivered, as is
// anything sharing the boundary millisecond that the cursor names. A
// cursor pointing past the end of history yields len(hist) — an empty
// final page, which is the honest answer when deploys were scrubbed
// between two page reads.
func skipDelivered(hist []registry.DeployEvent, cur telemetry.Cursor) int {
	excluded := make(map[string]bool, len(cur.ExcludeIDs))
	for _, id := range cur.ExcludeIDs {
		excluded[id] = true
	}
	for i := range hist {
		ms := hist[i].DeployedAt.UnixMilli()
		if ms > cur.BeforeMS {
			continue // newer than the boundary — already delivered
		}
		if ms == cur.BeforeMS && excluded[hist[i].ID] {
			continue // delivered on the boundary instant
		}
		return i
	}
	return len(hist)
}

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

	var cur telemetry.Cursor
	paging := false
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		parsed, err := telemetry.DecodeCursor(raw)
		if err != nil {
			// Same reasoning as the logs cursor: a malformed cursor must
			// not silently reset to the newest page, which would show the
			// reader the rows they already have with nothing to explain
			// why.
			writeErr(w, http.StatusBadRequest, "cursor is not a valid pagination token: "+err.Error())
			return
		}
		cur, paging = parsed, true
	}

	// The whole (store-capped) history, not limit+1 rows.
	//
	// This costs no more than the old call did: the colocated store LISTs
	// the prefix and opens every object before it sorts, so the limit
	// argument never reduced any IO — it only truncated the result.
	// Paging needs the sorted whole to locate a cursor, and the store's
	// own MaxHistoryScan is the real bound.
	hist, err := c.Registry.DeployHistory(r.Context(), stack, 0)
	if err != nil {
		slog.Error("deploy history read failed", "stack", stack, "err", err)
		writeErr(w, http.StatusInternalServerError, "deploy history read failed")
		return
	}

	start := 0
	if paging {
		start = skipDelivered(hist, cur)
	}
	end := min(start+limit, len(hist))
	page := hist[start:end]

	// One event beyond the page is used ONLY as the oldest row's diff
	// baseline. Without it the last row on every page would report
	// "nothing to compare against" — technically true of the page, and
	// a lie about the service.
	hasBaseline := end < len(hist)

	// Snapshot memo for THIS request. Consecutive rows share a snapshot
	// (row i's baseline is row i+1's own config), so without it a 20-row
	// page spends ~40 object GETs where 21 suffice. Paging made this
	// worth doing: the reader can now walk the whole history.
	snaps := snapshotCache{reg: c.Registry, stack: stack}

	out := make([]deployJSON, 0, len(page))
	for i, ev := range page {
		row := toDeployJSON(ev)

		// hist is newest-first, so the PREVIOUS deploy is the next index.
		var prev *registry.DeployEvent
		if start+i+1 < len(hist) {
			prev = &hist[start+i+1]
		}
		d := c.diffFor(r, stack, ev, prev, !hasBaseline && i == len(page)-1, &snaps)
		row.Diff = &d
		out = append(out, row)
	}

	resp := map[string]any{
		"stack": stack,
		"count": len(out),
		// `total` is what the store could see, so the UI can say "20 of
		// 63" instead of implying one page is the whole history.
		"total":   len(hist),
		"deploys": out,
	}
	if hasBaseline && len(page) > 0 {
		// hist[:end], not page: see deployCursorFor on why the exclusion
		// set spans the whole walk.
		resp["next_cursor"] = deployCursorFor(page[len(page)-1], hist[:end])
	}
	// The store's scan cap is indistinguishable from the end of history
	// unless it is named, and the row that runs into it is the oldest one
	// a reader ever reaches. Reported on every page so the statement is
	// there before they get that far.
	if len(hist) >= registry.MaxHistoryScan {
		resp["capped"] = true
		resp["cap_reason"] = fmt.Sprintf(
			"this timeline covers the newest %d deploys recorded for this service; anything older is not listed",
			registry.MaxHistoryScan,
		)
	}
	writeJSON(w, http.StatusOK, resp)
}

// snapshotCache memoizes one request's config-snapshot reads.
//
// Scoped to a single request deliberately: a process-lifetime cache
// would be a correctness hazard — a snapshot can be rewritten by a
// re-register, and this process may sit frozen between invocations for
// hours — while the within-request sharing is guaranteed by the
// timeline's own shape.
type snapshotCache struct {
	reg   registry.Store
	stack string
	seen  map[string]snapshotResult
}

type snapshotResult struct {
	cfg   map[string]any
	found bool
	err   error
}

func (s *snapshotCache) get(r *http.Request, eventID string) (map[string]any, bool, error) {
	if got, ok := s.seen[eventID]; ok {
		return got.cfg, got.found, got.err
	}
	cfg, found, err := s.reg.DeployConfig(r.Context(), s.stack, eventID)
	if s.seen == nil {
		s.seen = make(map[string]snapshotResult)
	}
	s.seen[eventID] = snapshotResult{cfg: cfg, found: found, err: err}
	return cfg, found, err
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
	isOldestOverall bool, snaps *snapshotCache,
) configdiff.Diff {
	if prev == nil {
		if isOldestOverall {
			return configdiff.Unavailable("first recorded deploy — there is nothing before it to compare against")
		}
		// Reachable only when the store's scan cap cut the history here,
		// since a page always carries one event past its end as the
		// baseline. Paging cannot fix it, so the message no longer
		// suggests raising `limit`.
		return configdiff.Unavailable("the deploy before this one is older than this timeline reaches, so there is nothing to compare against")
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

	curCfg, curFound, err := snaps.get(r, ev.ID)
	if err != nil || !curFound {
		if err != nil {
			slog.Warn("deploy config read failed", "stack", stack, "event", ev.ID, "err", err)
		}
		return configdiff.Unavailable("this deploy's config snapshot could not be read")
	}
	prevCfg, prevFound, err := snaps.get(r, prev.ID)
	if err != nil || !prevFound {
		if err != nil {
			slog.Warn("deploy config read failed", "stack", stack, "event", prev.ID, "err", err)
		}
		return configdiff.Unavailable("the previous deploy's config snapshot could not be read")
	}
	return configdiff.Compute(prevCfg, curCfg)
}
