// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// Store persists the service registry and its deploy history.
//
// Two implementations ship: InMemory (tests, local dev) and Colocated
// (JSON objects under a `_meta/` prefix in the storage bucket — the
// default, so an operator who names only a bucket needs no second
// store). Database-backed impls follow the same interface.
type Store interface {
	// Get returns one record. found=false with no error when absent.
	Get(ctx context.Context, stack string) (ServiceRecord, bool, error)

	// List returns every record, sorted by Stack for stable rendering.
	List(ctx context.Context) ([]ServiceRecord, error)

	// Put writes a record verbatim. Callers should prefer Register,
	// which applies the merge rules.
	Put(ctx context.Context, rec ServiceRecord) error

	// Delete removes a record and its deploy history. Idempotent — a
	// missing stack is not an error.
	Delete(ctx context.Context, stack string) error

	// AppendDeployEvent records one registration. Idempotent on
	// DeployEvent.ID so a retried push does not double-count.
	AppendDeployEvent(ctx context.Context, ev DeployEvent) error

	// DeployHistory returns events for one stack, newest first.
	// limit <= 0 means no cap.
	DeployHistory(ctx context.Context, stack string, limit int) ([]DeployEvent, error)

	// PutHealth records one probe outcome.
	PutHealth(ctx context.Context, res HealthResult) error

	// HealthHistory returns probe results for one stack, newest first.
	// limit <= 0 means no cap.
	HealthHistory(ctx context.Context, stack string, limit int) ([]HealthResult, error)

	// LatestHealth returns the most recent result per region for one
	// stack. Keyed by region ("" for a single-region service) so the
	// dashboard can show a per-region status rather than collapsing a
	// partial outage into one green tick.
	LatestHealth(ctx context.Context, stack string) (map[string]HealthResult, error)

	// PruneHealth drops results older than `before`. Probe results are
	// small but accumulate per service per interval forever.
	PruneHealth(ctx context.Context, before time.Time) (int, error)

	Close(ctx context.Context) error
}

// StalenessCutoff is how long a service may go without re-registering
// before the dashboard marks it stale.
//
// Deliberately generous. The plugin bus has no "consumer destroyed"
// event, so this is the ONLY automatic signal that a service may be
// gone — but a service that simply hasn't shipped in a week is not
// gone, and crying wolf on a healthy-but-quiet service would make the
// badge worthless. Explicit removal is DELETE /api/v1/services/{stack}.
const StalenessCutoff = 14 * 24 * time.Hour

// IsStale reports whether a record has gone quiet past the cutoff.
func (r ServiceRecord) IsStale(now time.Time) bool {
	if r.LastSeenAt.IsZero() {
		return false
	}
	return now.Sub(r.LastSeenAt) > StalenessCutoff
}

// ShortCommit returns a display-length commit, preferring the recorded
// full SHA and falling back to the short SHA embedded in SC's CalVer
// version string (`YYYY.MM.DD-<short-sha>`).
//
// SC injects VERSION on every image build but does NOT inject COMMIT, so
// a service that hasn't declared GITHUB_SHA under `image.build.args`
// still yields something useful here rather than a blank column.
func (r ServiceRecord) ShortCommit() string {
	if len(r.Commit) >= 7 {
		return r.Commit[:7]
	}
	if r.Commit != "" {
		return r.Commit
	}
	if i := strings.LastIndex(r.Version, "-"); i >= 0 && i+1 < len(r.Version) {
		return r.Version[i+1:]
	}
	return ""
}

// SplitStack splits an SC stack name into service and environment on
// the "--" separator the fleet uses ("forge-notifier--staging").
//
// A name without the separator is returned as the service with an empty
// env rather than guessed at — an operator whose naming differs should
// see their own name, not a mangled one.
func SplitStack(stack string) (service, env string) {
	if i := strings.Index(stack, "--"); i >= 0 {
		return stack[:i], stack[i+2:]
	}
	return stack, ""
}

// DeployEventID derives a stable id for one deploy.
//
// Keyed on stack + VERSION + deploy time. Version is in the key
// deliberately: keying on (stack, time) alone collapses two genuinely
// different versions that land in the same instant into one event, and
// the second is then silently dropped by the idempotency check. That is
// not a theoretical race — it is what the handler does whenever its
// clock is coarse, and a dashboard that silently loses a deploy from
// the timeline is worse than one that shows a duplicate.
//
// Retry protection does not depend on this: Register only appends when
// the version actually changed, so a retried push-register sees its own
// already-persisted version and appends nothing. This id is the
// second layer.
func DeployEventID(stack, version string, deployedAt time.Time) string {
	sum := sha256.Sum256([]byte(stack + "|" + version + "|" + deployedAt.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])[:32]
}

// Register applies the registry's merge rules and writes the record
// plus its deploy event.
//
// The merge rules exist because of a live incident on the first SC
// Component: sc-component-backup's register handler blind-replaced its
// tenant record, so an UNRELATED deploy whose sibling-discovery came up
// empty wiped the connection details of the fleet's largest database
// and silently stopped backing it up for 90 minutes with no alert.
//
// The lesson generalises: a registration carries what THIS deploy
// observed, which is not always everything that is true. So:
//
//   - FirstSeenAt is preserved from the stored record — a re-deploy must
//     not reset a service's age.
//   - Empty Sources never overwrite non-empty stored Sources. Telemetry
//     discovery is best-effort (and inferred outright until decision D1
//     lands); losing a known-good handle to a transient miss would make
//     logs vanish from the dashboard for no visible reason.
//   - A DeployEvent is appended only when Version actually changed, so
//     the timeline shows deploys rather than every no-op re-register.
func Register(ctx context.Context, store Store, incoming ServiceRecord, now time.Time) (ServiceRecord, error) {
	now = now.UTC()
	rec := incoming
	rec.Service, rec.Env = SplitStack(rec.Stack)
	rec.LastSeenAt = now
	if rec.DeployedAt.IsZero() {
		rec.DeployedAt = now
	}

	existing, found, err := store.Get(ctx, rec.Stack)
	if err != nil {
		return ServiceRecord{}, err
	}

	previousVersion := ""
	isNew := !found
	if found {
		previousVersion = existing.Version

		// Age is a property of the service, not of this deploy.
		rec.FirstSeenAt = existing.FirstSeenAt
		if rec.FirstSeenAt.IsZero() {
			rec.FirstSeenAt = existing.LastSeenAt
		}

		// Never trade a known-good telemetry handle for a discovery miss.
		if len(rec.Sources) == 0 && len(existing.Sources) > 0 {
			rec.Sources = existing.Sources
			rec.Inferred = existing.Inferred
		}

		// Likewise for the endpoint URL: a template that didn't publish
		// one this pass shouldn't blank a working link.
		if rec.EndpointURL == "" {
			rec.EndpointURL = existing.EndpointURL
		}
		if rec.Commit == "" {
			rec.Commit = existing.Commit
		}
		if rec.Repository == "" {
			rec.Repository = existing.Repository
		}
		// Same preserve-on-empty rule as Sources, for the same reason:
		// a registration reports what THIS deploy observed, and a
		// deploy that could not read the client config (or a plugin
		// version that did not send it) must not erase what a previous
		// one captured.
		if len(rec.ClientConfig) == 0 && len(existing.ClientConfig) > 0 {
			rec.ClientConfig = existing.ClientConfig
		}
		if len(rec.Regions) == 0 && len(existing.Regions) > 0 {
			rec.Regions = existing.Regions
		}
		if rec.HealthPath == "" {
			rec.HealthPath = existing.HealthPath
		}
	} else {
		rec.FirstSeenAt = now
	}

	if err := store.Put(ctx, rec); err != nil {
		return ServiceRecord{}, err
	}

	// A version change is a deploy — and so is the FIRST registration,
	// whatever the version says.
	//
	// The `isNew` half is not redundant. Deploy-time version is not
	// always knowable: the plugin bus carries no version field, so a
	// component plugin can only read VERSION out of the env preceding
	// `uses:` entries contributed, and a consumer that passes no build
	// arg registers with Version "". The first registration then
	// compared "" against "" and recorded NO deploy event at all — the
	// deploy history stayed empty for a service that had demonstrably
	// just deployed (live-caught 2026-11-19 on forge-storage, the first
	// tenant: `deploys: 0` on a record whose `deployed_at` was two
	// minutes old).
	//
	// A deploy we can timestamp but not version is still a deploy, and
	// recording it with an empty version is strictly more honest than
	// pretending it never happened.
	if isNew || rec.Version != previousVersion {
		ev := DeployEvent{
			ID:              DeployEventID(rec.Stack, rec.Version, rec.DeployedAt),
			Stack:           rec.Stack,
			Version:         rec.Version,
			Commit:          rec.Commit,
			Template:        rec.Template,
			Cloud:           rec.Cloud,
			DeployedAt:      rec.DeployedAt,
			PreviousVersion: previousVersion,
		}
		if err := store.AppendDeployEvent(ctx, ev); err != nil {
			return ServiceRecord{}, err
		}
	}

	return rec, nil
}

// SortRecords orders records for stable rendering: by service, then
// environment, then stack.
func SortRecords(recs []ServiceRecord) {
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Service != recs[j].Service {
			return recs[i].Service < recs[j].Service
		}
		if recs[i].Env != recs[j].Env {
			return recs[i].Env < recs[j].Env
		}
		return recs[i].Stack < recs[j].Stack
	})
}
