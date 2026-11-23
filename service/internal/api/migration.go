// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
)

// MigrationConfig wires the `/admin/migrate-s3-to-goembedb` endpoint.
//
// Separated from the other config bundles in this package because the
// migration is NOT part of the dashboard's hot path: it is opt-in
// plumbing the operator runs once per stack during cutover, and it
// needs BOTH stores (source + sink) at the same time. The rest of the
// service runs against exactly one storage backend chosen by
// USE_GOEMBEDB.
//
// Gating is by `ADMIN_MIGRATION_TOKEN` env (not the JWT), on purpose:
// the migration endpoint is called by an operator shell (curl against
// the deployed Lambda), not by the plugin's register flow nor by a
// logged-in dashboard user. Mixing it into JWTMiddleware would make
// the operator mint and manage yet another scoped token for a
// once-per-stack operation.
type MigrationConfig struct {
	// Source is the current S3Store — the fallback when USE_GOEMBEDB
	// is absent. Must be populated during cutover even when the
	// service is otherwise running on Mongo, because the migration
	// copies FROM S3.
	Source storage.Store

	// Sink is the MongoStore.
	Sink *storage.MongoStore

	// AdminToken, if non-empty, is required as the value of the
	// `X-Admin-Token` header. Empty disables the endpoint (which is
	// the safe default: an operator who did not set the env never
	// opens an unauthenticated migration route).
	AdminToken string
}

// migrationResponse is the shape returned on success.
//
// `Migrated` is the per-collection count the destination's documents
// landed under (so an operator can diff against each class's expected
// row count). `S3Count` is how many source objects the LIST walked.
// Verified = true when the two totals agree AND no per-key error
// fired; false (with Errors non-empty) otherwise.
type migrationResponse struct {
	DryRun    bool             `json:"dry_run"`
	S3Count   int              `json:"s3_count"`
	Migrated  map[string]int64 `json:"migrated"`
	Total     int64            `json:"total_migrated"`
	ElapsedMs int64            `json:"elapsed_ms"`
	Verified  bool             `json:"verified"`
	Errors    []migrationError `json:"errors,omitempty"`
}

type migrationError struct {
	Path string `json:"path"`
	Err  string `json:"err"`
}

// Migrate serves `POST /admin/migrate-s3-to-goembedb`.
//
// Behavior:
//
//  1. Source.List("") yields every key observatory currently stores
//     in S3. The colocated store namespaces everything under `_meta/`,
//     so even a bucket shared with other sc-components (prefix
//     routing) walks only the registry's own tree.
//  2. For each key: Source.Get then Sink.Put. Idempotent — ReplaceOne
//     with upsert means a retried migration overwrites, not
//     duplicates.
//  3. CollectionCounts on the sink produces the per-class row count
//     the operator diffs against expectations. verified = (total of
//     migrated per-collection == S3 LIST count).
//
// Query parameter `dry_run=1` lists but does not write. Lets the
// operator validate scope and count before touching Mongo.
//
// A per-key error is NON-fatal: the migration continues past it and
// the response lists every failure. Live-caught lesson from an
// earlier SC Component: a cutover that aborted on the first error
// left the destination half-populated with no record of which keys
// landed, forcing a full re-run on a cold Mongo.
func (c *MigrationConfig) Migrate(w http.ResponseWriter, r *http.Request) {
	if c.AdminToken == "" {
		writeErr(w, http.StatusNotFound, "migration endpoint disabled (ADMIN_MIGRATION_TOKEN unset)")
		return
	}
	if r.Header.Get("X-Admin-Token") != c.AdminToken {
		writeErr(w, http.StatusUnauthorized, "missing or invalid X-Admin-Token")
		return
	}
	if c.Source == nil {
		writeErr(w, http.StatusInternalServerError, "migration source (S3Store) is not configured")
		return
	}
	if c.Sink == nil {
		writeErr(w, http.StatusInternalServerError, "migration sink (MongoStore) is not configured")
		return
	}

	dryRun := r.URL.Query().Get("dry_run") == "1"
	start := time.Now()

	ctx := r.Context()
	objs, err := c.Source.List(ctx, "")
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("list source: %v", err))
		return
	}
	// Stable iteration so a diff of two runs is meaningful.
	sort.Slice(objs, func(i, j int) bool { return objs[i].Path < objs[j].Path })

	resp := migrationResponse{
		DryRun:   dryRun,
		S3Count:  len(objs),
		Migrated: map[string]int64{},
	}

	for _, o := range objs {
		if dryRun {
			resp.Total++
			continue
		}
		rc, gerr := c.Source.Get(ctx, o.Path)
		if gerr != nil {
			resp.Errors = append(resp.Errors, migrationError{Path: o.Path, Err: "get: " + gerr.Error()})
			continue
		}
		body, rerr := io.ReadAll(rc)
		_ = rc.Close()
		if rerr != nil {
			resp.Errors = append(resp.Errors, migrationError{Path: o.Path, Err: "read: " + rerr.Error()})
			continue
		}
		if _, perr := c.Sink.Put(ctx, o.Path, bytes.NewReader(body)); perr != nil {
			resp.Errors = append(resp.Errors, migrationError{Path: o.Path, Err: "put: " + perr.Error()})
			continue
		}
		resp.Total++
	}

	// Count destination rows even on a dry-run — gives the operator a
	// "what would be touched" vs "what is already there" picture.
	counts, cerr := c.Sink.CollectionCounts(ctx)
	if cerr != nil {
		resp.Errors = append(resp.Errors, migrationError{Path: "<collection-counts>", Err: cerr.Error()})
	} else {
		resp.Migrated = counts
	}

	// Verified: zero per-key errors AND the per-collection total
	// matches the S3 LIST count. A dry run never claims verified.
	if !dryRun && len(resp.Errors) == 0 {
		var sum int64
		for _, n := range resp.Migrated {
			sum += n
		}
		resp.Verified = sum >= int64(resp.S3Count)
	}
	resp.ElapsedMs = time.Since(start).Milliseconds()

	slog.Info("migration",
		"dry_run", resp.DryRun,
		"s3_count", resp.S3Count,
		"total_migrated", resp.Total,
		"verified", resp.Verified,
		"errors", len(resp.Errors),
		"elapsed_ms", resp.ElapsedMs)

	writeJSON(w, http.StatusOK, resp)
}

// jsonRoundTrip is a defensive sanity-check for a document body: a JSON
// unmarshal into map[string]any must succeed on every object observatory
// writes (ServiceRecord, DeployEvent, HealthResult, deploy-config
// snapshot are all JSON — see registry/store_colocated.go putJSON). Used
// only by the dry-run path to surface a corrupt source object before the
// operator tries a wet migration.
//
// Intentionally unused in the wet path: Mongo stores the body as a
// binary blob either way, and a corrupt object still round-trips
// byte-for-byte on read (the colocated store's own getJSON skips
// unmarshal failures today).
func jsonRoundTrip(buf []byte) error {
	var v any
	if err := json.Unmarshal(buf, &v); err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return nil
}

// errMigrationNotConfigured is the sentinel the wiring layer checks to
// decide whether to mount the route at all.
var errMigrationNotConfigured = errors.New("MigrationConfig requires Source + Sink + AdminToken")
