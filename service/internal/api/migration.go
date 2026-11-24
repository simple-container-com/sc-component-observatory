// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
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

	// ExcludePrefix is the SINK's own replication prefix inside the
	// source bucket, and keys under it are not migration input.
	// Normally storage.DefaultGoEmbedS3Prefix, resolved once at boot
	// (cmd/observatory: goEmbedS3Prefix) so boot and migration cannot
	// disagree. Empty means "exclude nothing" — correct for a source
	// bucket that is not also the replication target, and the right
	// default for tests that construct a MigrationConfig directly.
	//
	// See excludeReplicationKeys for why this is not optional in the
	// deployed configuration.
	ExcludePrefix string
}

// migrationResponse is the shape returned on success.
//
// `Migrated` is the per-collection count the destination's documents
// landed under (so an operator can diff against each class's expected
// row count). `S3Count` is how many source objects the LIST walked.
// `Verified` is deliberately conservative — see verifyAgainstSource and
// the comment at the assignment.
type migrationResponse struct {
	DryRun bool `json:"dry_run"`
	// Verify is true when the call only compared source to sink and
	// copied nothing. Distinct from DryRun, which walks the copy loop's
	// control flow without its I/O.
	Verify bool `json:"verify,omitempty"`
	// S3Count is how many source objects are IN SCOPE — the raw LIST
	// minus ExcludedReplication. The two together reconstruct what the
	// bucket actually holds, which matters because the raw number is
	// the one an operator sees from `aws s3 ls` and it will be larger.
	S3Count int `json:"s3_count"`
	// ExcludedReplication is how many listed keys were skipped as the
	// sink's own replication records. Reported rather than silently
	// subtracted: a reader comparing s3_count against a bucket count
	// needs to see where the difference went, and a sudden jump here is
	// the signal that the sink is writing more than it is being asked
	// to store. Omitted when zero.
	ExcludedReplication int              `json:"excluded_replication,omitempty"`
	Migrated            map[string]int64 `json:"migrated"`
	Total               int64            `json:"total_migrated"`
	// SinkRows is the sum over Migrated — the destination's TOTAL row
	// count, including anything that was there before this run. Exposed
	// because it is one half of what `verified` compares, and an
	// operator reading a bare `verified: true` cannot otherwise see
	// whether the sink was empty beforehand.
	SinkRows int64 `json:"sink_rows"`

	// SourceByCollection buckets the SOURCE listing by the collection
	// each key would land in. The per-class counterpart to Migrated, and
	// the only reading that distinguishes "9105 rows arrived" from "the
	// 9105 rows that arrived are the ones the dashboard needs".
	SourceByCollection map[string]int64 `json:"source_by_collection,omitempty"`
	// MissingByCollection holds, per collection, how many rows the sink
	// is short of what the source listed. Omitted when empty — an empty
	// map IS the success case, and a reader should not have to tell
	// "nothing missing" from "never computed".
	MissingByCollection map[string]int64 `json:"missing_by_collection,omitempty"`

	// Limit and After echo the chunking inputs, so a response pasted
	// into a handoff carries the request that produced it.
	Limit int    `json:"limit,omitempty"`
	After string `json:"after,omitempty"`
	// NextAfter is the last key this call PROCESSED — feed it back as
	// `after` to continue. Empty when the call copied nothing.
	NextAfter string `json:"next_after,omitempty"`
	// Done is true when this call reached the end of the listing, i.e.
	// no key remains after NextAfter. The operator's loop condition.
	Done bool `json:"done"`
	// Remaining is how many listed keys this call did not reach.
	Remaining int `json:"remaining"`
	// CopyMs is the copy loop alone, excluding the LIST and the sink
	// count. This is the number that sizes the next chunk; ElapsedMs
	// includes fixed per-call overhead and would over-estimate.
	CopyMs int64 `json:"copy_ms"`

	ElapsedMs int64            `json:"elapsed_ms"`
	Verified  bool             `json:"verified"`
	Errors    []migrationError `json:"errors,omitempty"`
}

// verifyAgainstSource compares a source listing, bucketed by destination
// collection, against the sink's actual per-collection row counts.
//
// Returns the per-collection shortfall (only non-zero entries) and
// whether the sink is sufficient for every class the source carries.
//
// This is a stronger check than comparing the two totals, and the
// difference is the exact failure this migration is exposed to. A
// partial copy is not a uniform sample of the source: it is a prefix of
// whatever order the loop walks, so which CLASS is missing is decided
// entirely by that order. Under plain sorted-path order —
// `deploy_configs` < `deploys` < `health` < `kv` < `services` — the
// bulkiest class (`health`, continuously written ticks) copies in the
// middle and `services`, the one the dashboard renders, copies LAST. A
// copy that died most of the way through would produce a sink whose
// total row count looked nearly complete with the dashboard empty, and
// an aggregate comparison reports that as a near-miss rather than the
// total failure it is. migrationOrder below fixes the order; this
// function is what makes the result legible either way.
//
// What this does NOT establish: that the rows in the sink got there
// from this migration. It compares a listing to a row count, so a sink
// that was already populated satisfies it trivially. Causation comes
// only from knowing the sink was empty beforehand, which is why the
// dry-run reports `sink_rows` and why the runbook says to read it first.
func verifyAgainstSource(sourceByCollection, sinkCounts map[string]int64) (missing map[string]int64, ok bool) {
	missing = map[string]int64{}
	for coll, want := range sourceByCollection {
		if got := sinkCounts[coll]; got < want {
			missing[coll] = want - got
		}
	}
	return missing, len(missing) == 0
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
//  2. For each key in the current chunk: Source.Get then Sink.Put.
//     Idempotent — ReplaceOne with upsert means a retried migration
//     overwrites, not duplicates.
//  3. CollectionCounts on the sink produces the per-class row count,
//     compared against the source listing bucketed the same way (see
//     verifyAgainstSource).
//
// Query parameters:
//
//	dry_run=1     list and bucket, write nothing. Validates scope and
//	              count, and reports the sink's PRE-migration row
//	              counts — the only reading that later establishes the
//	              migration is what populated it.
//	verify=1      compare source listing to sink counts, copy nothing.
//	              The pass that closes out a chunked migration.
//	limit=<n>     copy at most n keys this call. 0/absent = all of them.
//	after=<key>   skip keys <= this one. Feed back `next_after`.
//
// ## Why chunking exists
//
// The copy is serial — one GetObject plus one sink Put per key — and
// this service is a Lambda with a 30-second timeout behind a Cloudflare
// edge that gives up around 100 seconds. Neither bound is reachable
// from inside the handler, and the response is a single buffered write
// after the loop, so a run that outlives either one returns nothing at
// all: no count, no error list, no cursor. Worse, `verified` requires
// Total == S3Count against THIS call's copies, so re-invoking an
// unbounded migration restarts at the first key and makes no forward
// progress. A bucket that takes longer than the timeout can therefore
// never be migrated by retrying, only by chunking.
//
// Picking the chunk size is the operator's, from measurement: run one
// small `limit` and read `copy_ms`. The per-key cost is not derivable
// from any pre-cutover measurement — those were all taken against the
// S3 read path, and the sink here is go-embedb with S3 replication,
// which nothing pre-cutover touched.
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
	// Constant-time compare so a remote attacker cannot learn the
	// token's length or prefix by timing the response. Low severity
	// (shared bearer, remote timing), trivially cheap, brings
	// observatory consistent with notifier + backup#41.
	got := r.Header.Get("X-Admin-Token")
	if subtle.ConstantTimeCompare([]byte(got), []byte(c.AdminToken)) != 1 {
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

	q := r.URL.Query()
	dryRun := q.Get("dry_run") == "1"
	verifyOnly := q.Get("verify") == "1"
	if dryRun && verifyOnly {
		writeErr(w, http.StatusBadRequest, "dry_run and verify are mutually exclusive: dry_run walks the copy loop without I/O, verify skips the loop entirely")
		return
	}
	// A malformed limit is refused rather than defaulted, because
	// `limit = 0` means UNBOUNDED here — so falling through on a parse
	// error does not degrade to a conservative chunk, it runs the whole
	// bucket. TestMigration_BadParams was mutated to confirm that: with
	// the guard disabled, `limit=abc` copied every fixture instead of
	// returning 400. On the real bucket (9105 objects) that is the
	// unbounded, un-resumable run that outlives the Lambda timeout and
	// returns nothing — exactly what chunking exists to avoid, reached
	// by a typo.
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("limit must be a non-negative integer, got %q", raw))
			return
		}
		limit = n
	}
	after := q.Get("after")
	start := time.Now()

	ctx := r.Context()
	objs, err := c.Source.List(ctx, "")
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("list source: %v", err))
		return
	}
	objs, excluded := excludeReplicationKeys(objs, c.ExcludePrefix)
	migrationOrder(objs)

	resp := migrationResponse{
		DryRun:              dryRun,
		Verify:              verifyOnly,
		S3Count:             len(objs),
		Migrated:            map[string]int64{},
		SourceByCollection:  map[string]int64{},
		Limit:               limit,
		After:               after,
		ExcludedReplication: excluded,
	}
	for _, o := range objs {
		resp.SourceByCollection[storage.CollectionNameFor(o.Path)]++
	}

	// Resolve the cursor to an index in THIS call's ordering. Index, not
	// a lexical `>` comparison, because migrationOrder is not lexical —
	// a string compare against `after` would silently restart inside a
	// different class.
	//
	// A cursor that is no longer in the listing is a 409, not a silent
	// restart from zero: the source changed under the operator (the
	// health prune sweep deletes keys), and resuming from the top would
	// re-copy everything already done while reporting a `total_migrated`
	// that double-counts. Loud is cheap here — `verify=1` tells them
	// where they actually stand.
	startIdx := 0
	if after != "" {
		idx := -1
		for i, o := range objs {
			if o.Path == after {
				idx = i
				break
			}
		}
		if idx < 0 {
			writeErr(w, http.StatusConflict, fmt.Sprintf("cursor %q is not in the current source listing — it was deleted or the source changed; run with verify=1 to see what the sink is missing, then resume from a key that still exists or re-run without `after`", after))
			return
		}
		startIdx = idx + 1
	}

	pending := objs[startIdx:]
	if verifyOnly {
		// Nothing is copied, so there is no cursor to advance and
		// nothing is pending. Done reflects the comparison having
		// covered the whole listing, which it always does.
		resp.Done = true
	} else {
		chunk := pending
		if limit > 0 && limit < len(chunk) {
			chunk = chunk[:limit]
		}
		copyStart := time.Now()
		for _, o := range chunk {
			// NextAfter advances on every key the loop REACHES,
			// including one that errored. A failed key that blocked the
			// cursor would make the operator's loop spin on it forever;
			// the failure is in `errors` and the shortfall is in
			// `missing_by_collection`, so it cannot be lost by moving on.
			resp.NextAfter = o.Path
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
		resp.CopyMs = time.Since(copyStart).Milliseconds()
		resp.Remaining = len(pending) - len(chunk)
		resp.Done = resp.Remaining == 0
	}

	// Count destination rows even on a dry-run — gives the operator a
	// "what would be touched" vs "what is already there" picture.
	counts, cerr := c.Sink.CollectionCounts(ctx)
	if cerr != nil {
		resp.Errors = append(resp.Errors, migrationError{Path: "<collection-counts>", Err: cerr.Error()})
	} else {
		resp.Migrated = counts
	}

	for _, n := range resp.Migrated {
		resp.SinkRows += n
	}

	missing, sinkSufficient := verifyAgainstSource(resp.SourceByCollection, resp.Migrated)
	if len(missing) > 0 {
		resp.MissingByCollection = missing
	}

	// Verified: a non-empty source, no errors in this call, the whole
	// listing covered, and EVERY class in the sink at least as populated
	// as the source listed for it.
	//
	// Each clause is here because the obvious one-liner this replaced
	// (`sum >= S3Count`) was computable as TRUE in three ways that had
	// nothing to do with a good migration, and `verified` is the single
	// field the runbook tells the operator to read:
	//
	//   - **Empty source.** 0 >= 0. A misconfigured Source (wrong
	//     bucket, wrong prefix, credentials that list nothing) reports
	//     success. Hence `S3Count > 0`.
	//   - **Truncated source.** 1000 copied >= 1000 listed, when the
	//     bucket held more. That was real: an unpaginated
	//     `S3Store.List` clipped at the ListObjectsV2 page cap. The real
	//     count turned out to be 9105, so the clip was dropping ~89% —
	//     and under the old sorted-path order it would have copied
	//     `health/` ticks and never reached `services/`. Fixed in
	//     storage/s3.go; `S3Count > 0` cannot catch it, so the fix is
	//     the lister's and the warn below is the tripwire.
	//   - **A sink that was already populated.** `sum` is the
	//     DESTINATION's whole row count, not a tally of this run's work,
	//     so a sink carrying live traffic satisfies `sum >= S3Count`
	//     even if zero objects copied. The per-class comparison has the
	//     same blind spot by construction — it compares a listing to row
	//     counts — which is why `sink_rows` is reported on the dry-run:
	//     the PRE-migration reading is the only thing that establishes
	//     this migration is what populated the sink.
	//
	// Deliberately NOT in the formula: `Total == S3Count`. Under
	// chunking it is false on every chunk but the whole-bucket one, so
	// requiring it would mean a chunked migration could never report
	// verified even once complete. `sinkSufficient` is the stronger
	// statement anyway — it is about the store's contents rather than
	// one call's tally, and it is the one an operator actually wants
	// before dropping the migration token. A dry run never claims
	// verified; `verify=1` is exactly how you ask this question without
	// copying.
	if !dryRun && resp.S3Count > 0 && len(resp.Errors) == 0 && resp.Done && sinkSufficient {
		resp.Verified = true
	}
	resp.ElapsedMs = time.Since(start).Milliseconds()

	// A source count that is an exact multiple of 1000 is the signature
	// of a list operation that stopped at a page boundary — AWS caps a
	// ListObjectsV2 page at 1000 keys and signals the clip only via
	// IsTruncated, which an unpaginated caller never reads. The real
	// count landing on a round 1000 is possible but unlikely; a
	// truncated one is certain to. Not a failure: the operator is the
	// one who can tell which it is, and this is how they get asked.
	if resp.S3Count > 0 && resp.S3Count%1000 == 0 {
		slog.Warn("migration source count is an exact multiple of 1000 — may be a truncated (unpaginated) listing rather than a real count; audit the source lister before trusting `verified`",
			"s3_count", resp.S3Count)
	}

	slog.Info("migration",
		"dry_run", resp.DryRun,
		"s3_count", resp.S3Count,
		"total_migrated", resp.Total,
		"verified", resp.Verified,
		"errors", len(resp.Errors),
		"elapsed_ms", resp.ElapsedMs)

	writeJSON(w, http.StatusOK, resp)
}

// migrationRank orders the destination collections by how much a
// partial migration hurts if that class is the one left behind.
//
// Lower copies first. This is NOT arbitrary and it is NOT alphabetical,
// which is the whole point:
//
//	services       the dashboard's primary index. Empty ⇒ the UI has
//	               nothing to render at all, and it is the smallest
//	               class, so it costs nothing to front-load.
//	deploy_configs small, and what the config-diff view reads.
//	deploys        the timeline. Degrades gracefully — a short timeline
//	               looks like a young service.
//	kv             miscellany; nothing renders it directly.
//	health         bulkiest by a wide margin (one object per tick per
//	               stack) and the most self-healing: the scheduler
//	               writes new ticks continuously, so a gap backfills
//	               itself with time. Last by both measures.
//
// Why it matters: a partial copy is a PREFIX of the walk order, so the
// order decides which class survives a bound being hit. Plain sorted
// order puts `services` last and `health` in the middle — exactly
// inverted. Under it, a copy clipped at 1000 of 9105 keys would have
// migrated nothing but health ticks and reported a plausible-looking
// row count over an empty dashboard.
//
// The sibling sc-component-backup has the same hazard with a different
// cast: its five walked prefixes are alphabetical and `tenants/` sorts
// LAST, while its scheduler treats a tenant with no row as never backed
// up — so a clipped copy there does not merely look empty, it can
// re-fire backups. Any component adding a migration of this shape
// should pick the order deliberately rather than inherit the sort.
var migrationRank = map[string]int{
	storage.CollectionServices:      0,
	storage.CollectionDeployConfigs: 1,
	storage.CollectionDeploys:       2,
	storage.CollectionKV:            3,
	storage.CollectionHealth:        4,
}

// excludeReplicationKeys drops the keys the SINK wrote, and returns how
// many it dropped. An empty prefix is a no-op.
//
// ## Why this exists: the migration was reading the sink's write-ahead
// log as if it were pending work
//
// Live-caught 2026-11-24, during the observatory wet run, within twenty
// minutes of the USE_GOEMBEDB=1 flip. Three individually safe decisions
// composed into one:
//
//  1. GOEMBEDB_S3_BUCKET deliberately points at the bucket the
//     component ALREADY owns — the same bucket this migration reads
//     from — so that the cutover creates no new paid resource and needs
//     no new IAM grant. Frugal, and the right call.
//  2. storage.CollectionNameFor routes any path outside the `_meta/`
//     tree to the catch-all `kv` collection, so a future writer using
//     an unforeseen key shape is still served. Defensive, and correct.
//  3. The source enumeration is Source.List(ctx, "") — the WHOLE
//     bucket, because a migration that silently skipped a prefix would
//     be the worse failure. Complete, and deliberate.
//
// Together: every row go-embedb accepted replicated back into the same
// bucket under its own prefix; that prefix matches no `_meta/` class,
// so it classified as `kv`; and the unfiltered LIST handed it back as
// migration input. Measured: `kv` went from 3 source keys at dry-run to
// **9166** eighteen minutes later, against ~9127 rows written — a 1:1
// echo of the migration's own output. `s3_count` went 9102 -> 18265.
//
// Two consequences, and the second is the one that cost time:
//
//   - The run copied 25 replication records INTO the kv collection,
//     each of which generated another replication record. Bounded per
//     call by the chunk cursor, but every call grew the source.
//   - `verified` became a false negative that could never go true.
//     `missing_by_collection: {kv: 9138}` is not a shortfall, it is
//     out-of-scope data counted as scope — and it is unfixable by
//     copying harder, because copying is what produces it.
//
// The per-class verify hardened earlier that same day did its job and
// still reported failure, because the question it answers ("is the sink
// sufficient for every class the source carries?") is sound only if the
// source set is sound. A correct formula over a contaminated input set
// returns a confident wrong answer.
//
// Note what did NOT catch this. The server.yaml comment reasoned the
// prefix "cannot collide with the S3Store's own `_meta/` records in the
// same bucket" — true, and true only for READS. The enumeration is not
// a read of a known key; it is a question about the whole bucket, and a
// prefix that collides with nothing still answers it.
func excludeReplicationKeys(objs []storage.StoredObject, prefix string) (kept []storage.StoredObject, excluded int) {
	if prefix == "" {
		return objs, 0
	}
	kept = make([]storage.StoredObject, 0, len(objs))
	for _, o := range objs {
		if strings.HasPrefix(o.Path, prefix) {
			excluded++
			continue
		}
		kept = append(kept, o)
	}
	return kept, excluded
}

// migrationOrder sorts objs in place into the copy order described on
// migrationRank: by class rank, then by path within a class.
//
// Deterministic and total — the path tiebreak matters because the
// chunking cursor is an index into this ordering, so two calls over the
// same listing must produce the same sequence or a resume lands in the
// wrong place. An unranked class (impossible today: CollectionNameFor
// falls back to `kv`) sorts after every ranked one rather than
// panicking.
func migrationOrder(objs []storage.StoredObject) {
	rank := func(path string) int {
		if r, ok := migrationRank[storage.CollectionNameFor(path)]; ok {
			return r
		}
		return len(migrationRank)
	}
	sort.SliceStable(objs, func(i, j int) bool {
		ri, rj := rank(objs[i].Path), rank(objs[j].Path)
		if ri != rj {
			return ri < rj
		}
		return objs[i].Path < objs[j].Path
	})
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
