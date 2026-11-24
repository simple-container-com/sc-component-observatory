// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/simple-container-com/sc-component-observatory/service/internal/api"
	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
)

// chunkResponse is the full migration response shape, decoded by the
// chunking tests. Kept separate from the partial anonymous structs the
// older tests use because these assertions turn on fields those tests
// predate.
type chunkResponse struct {
	DryRun              bool             `json:"dry_run"`
	Verify              bool             `json:"verify"`
	S3Count             int              `json:"s3_count"`
	Migrated            map[string]int64 `json:"migrated"`
	Total               int64            `json:"total_migrated"`
	SinkRows            int64            `json:"sink_rows"`
	SourceByCollection  map[string]int64 `json:"source_by_collection"`
	MissingByCollection map[string]int64 `json:"missing_by_collection"`
	Limit               int              `json:"limit"`
	After               string           `json:"after"`
	NextAfter           string           `json:"next_after"`
	Done                bool             `json:"done"`
	Remaining           int              `json:"remaining"`
	CopyMs              int64            `json:"copy_ms"`
	ElapsedMs           int64            `json:"elapsed_ms"`
	Verified            bool             `json:"verified"`
	Errors              []struct {
		Path string `json:"path"`
		Err  string `json:"err"`
	} `json:"errors"`
}

// migrateQ issues one migration call with the given query parameters
// and returns the status plus the decoded body. Body is only decoded on
// 200 — an error response has a different shape.
func migrateQ(t *testing.T, cfg *api.MigrationConfig, q url.Values) (int, chunkResponse, string) {
	t.Helper()
	target := "/admin/migrate-s3-to-goembedb"
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.Header.Set("X-Admin-Token", "secret-token")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)

	var resp chunkResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v (body %s)", err, w.Body.String())
		}
	}
	return w.Code, resp, w.Body.String()
}

// seedSource fills an InMemoryStore with the given keys, each body
// naming its own path so a round-trip mix-up is visible.
func seedSource(t *testing.T, keys []string) *storage.InMemoryStore {
	t.Helper()
	src := storage.NewInMemoryStore()
	for _, p := range keys {
		body := fmt.Sprintf(`{"path":%q}`, p)
		if _, err := src.Put(context.Background(), p, bytes.NewReader([]byte(body))); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	return src
}

func sinkHas(t *testing.T, sink *storage.MongoStore, path string) bool {
	t.Helper()
	rc, err := sink.Get(context.Background(), path)
	if err != nil {
		return false
	}
	_ = rc.Close()
	return true
}

// orderFixtures is chosen so LEXICAL order and MIGRATION order disagree
// on every adjacent pair that matters:
//
//	lexical:   deploy-configs < deploys < health < services
//	migration: services < deploy-configs < deploys < health
//
// So a test that pins the migration order cannot pass by accident on a
// sort that was left alphabetical, and a cursor implemented as a string
// comparison against `after` lands in a different class than one
// implemented as a position in the ordering.
var orderFixtures = []string{
	"_meta/deploy-configs/alpha/c1.json",
	"_meta/deploys/alpha/d1.json",
	"_meta/health/alpha/t1.json",
	"_meta/health/alpha/t2.json",
	"_meta/services/alpha.json",
}

// TestMigration_OrderCopiesServicesBeforeHealth is the regression test
// for the copy ORDER, which decides which class survives a chunk bound
// being hit. `services` is what the dashboard renders and `health` is
// the bulk; plain sorted-path order puts them exactly the wrong way
// round (`deploy-configs` first, `services` last).
//
// limit=1 is the sharpest possible probe: exactly one key copies, and
// which one it is answers the question with no arithmetic.
func TestMigration_OrderCopiesServicesBeforeHealth(t *testing.T) {
	src := seedSource(t, orderFixtures)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_chunk_order")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	code, resp, body := migrateQ(t, cfg, url.Values{"limit": {"1"}})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if resp.Total != 1 {
		t.Fatalf("total_migrated = %d, want 1", resp.Total)
	}
	if resp.NextAfter != "_meta/services/alpha.json" {
		t.Fatalf("next_after = %q, want _meta/services/alpha.json — the first key copied was not the services record, so the walk is still in sorted-path order", resp.NextAfter)
	}
	if !sinkHas(t, sink, "_meta/services/alpha.json") {
		t.Error("sink is missing the services record after a 1-key chunk")
	}
	if sinkHas(t, sink, "_meta/deploy-configs/alpha/c1.json") {
		t.Error("sink has the deploy-configs record after a 1-key chunk; that is the lexically-first key, not the first by migration order")
	}
	// The whole bucket is still listed and bucketed even though one key
	// copied — the source report is about the source, not the chunk.
	if resp.S3Count != len(orderFixtures) {
		t.Errorf("s3_count = %d, want %d", resp.S3Count, len(orderFixtures))
	}
	if resp.SourceByCollection[storage.CollectionHealth] != 2 {
		t.Errorf("source_by_collection[health] = %d, want 2", resp.SourceByCollection[storage.CollectionHealth])
	}
}

// TestMigration_PartialChunkNamesTheMissingClass — the reason the
// per-class comparison exists. After one chunk the sink holds rows and
// `sink_rows` is non-zero, which under an aggregate-only check reads as
// partial progress; what the operator needs to know is WHICH classes
// have not arrived, by name.
func TestMigration_PartialChunkNamesTheMissingClass(t *testing.T) {
	src := seedSource(t, orderFixtures)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_chunk_partial")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	code, resp, body := migrateQ(t, cfg, url.Values{"limit": {"2"}})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if resp.Verified {
		t.Fatal("verified = true after copying 2 of 5 keys")
	}
	if resp.Done {
		t.Fatal("done = true after copying 2 of 5 keys")
	}
	if resp.Remaining != 3 {
		t.Errorf("remaining = %d, want 3", resp.Remaining)
	}
	// services + deploy_configs are in; deploys + health are not.
	if got := resp.MissingByCollection[storage.CollectionHealth]; got != 2 {
		t.Errorf("missing_by_collection[health] = %d, want 2", got)
	}
	if got := resp.MissingByCollection[storage.CollectionDeploys]; got != 1 {
		t.Errorf("missing_by_collection[deploys] = %d, want 1", got)
	}
	if _, present := resp.MissingByCollection[storage.CollectionServices]; present {
		t.Errorf("missing_by_collection names services, but services copies first: %+v", resp.MissingByCollection)
	}
}

// TestMigration_ChunkedRunCopiesEverythingExactlyOnce drives the
// operator's loop: call, feed `next_after` back, repeat until `done`.
//
// Asserts the three properties the loop depends on — every key arrives,
// the per-chunk totals sum to the source count (so no key is copied
// twice and none is skipped), and `verified` turns true on the final
// chunk and not before.
func TestMigration_ChunkedRunCopiesEverythingExactlyOnce(t *testing.T) {
	src := seedSource(t, orderFixtures)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_chunk_loop")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	var (
		after    string
		sum      int64
		chunks   int
		lastResp chunkResponse
	)
	for {
		chunks++
		if chunks > 10 {
			t.Fatalf("loop did not terminate after %d chunks (last: %+v)", chunks, lastResp)
		}
		q := url.Values{"limit": {"2"}}
		if after != "" {
			q.Set("after", after)
		}
		code, resp, body := migrateQ(t, cfg, q)
		if code != http.StatusOK {
			t.Fatalf("chunk %d: status %d, body %s", chunks, code, body)
		}
		sum += resp.Total
		lastResp = resp
		if resp.Done {
			if !resp.Verified {
				t.Fatalf("chunk %d reported done but verified = false (missing=%+v errors=%+v)", chunks, resp.MissingByCollection, resp.Errors)
			}
			break
		}
		if resp.Verified {
			t.Fatalf("chunk %d reported verified = true while %d keys remain", chunks, resp.Remaining)
		}
		if resp.NextAfter == "" {
			t.Fatalf("chunk %d is not done but returned an empty next_after — the loop has no way to continue", chunks)
		}
		if resp.NextAfter == after {
			t.Fatalf("chunk %d returned the same cursor it was given (%q) — the loop would spin", chunks, after)
		}
		after = resp.NextAfter
	}

	if sum != int64(len(orderFixtures)) {
		t.Errorf("per-chunk totals sum to %d, want %d — a key was skipped or copied twice", sum, len(orderFixtures))
	}
	if chunks != 3 {
		t.Errorf("took %d chunks for 5 keys at limit=2, want 3", chunks)
	}
	for _, p := range orderFixtures {
		if !sinkHas(t, sink, p) {
			t.Errorf("sink is missing %q after the chunked run completed", p)
		}
	}
}

// TestMigration_CursorIsPositionalNotLexical is the test a string
// comparison cannot pass.
//
// `_meta/services/alpha.json` is LEXICALLY LAST among the fixtures but
// FIRST in migration order. So resuming from it must copy the next key
// by position (`_meta/deploy-configs/...`), whereas an implementation
// that filters `path > after` finds nothing at all and reports an
// immediate, wrong `done: true` over an unmigrated bucket.
func TestMigration_CursorIsPositionalNotLexical(t *testing.T) {
	src := seedSource(t, orderFixtures)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_chunk_cursor")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	q := url.Values{"limit": {"1"}, "after": {"_meta/services/alpha.json"}}
	code, resp, body := migrateQ(t, cfg, q)
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if resp.Total != 1 {
		t.Fatalf("total_migrated = %d, want 1 — a lexical `path > after` filter finds no key after the lexically-last one and copies nothing", resp.Total)
	}
	if resp.Done {
		t.Error("done = true after resuming from the first key of five")
	}
	if resp.NextAfter != "_meta/deploy-configs/alpha/c1.json" {
		t.Errorf("next_after = %q, want _meta/deploy-configs/alpha/c1.json (the key after services by migration order)", resp.NextAfter)
	}
	if resp.Remaining != 3 {
		t.Errorf("remaining = %d, want 3", resp.Remaining)
	}
}

// TestMigration_UnknownCursorIsConflict — a cursor that is no longer in
// the listing must be refused, not silently treated as "start from the
// beginning". The health prune sweep deletes keys, so this is reachable
// in production, and a silent restart would re-copy everything while
// reporting a total that double-counts.
func TestMigration_UnknownCursorIsConflict(t *testing.T) {
	src := seedSource(t, orderFixtures)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_chunk_badcursor")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	code, _, body := migrateQ(t, cfg, url.Values{"after": {"_meta/health/alpha/deleted.json"}})
	if code != http.StatusConflict {
		t.Fatalf("status %d, want 409 — body %s", code, body)
	}
	if !bytes.Contains([]byte(body), []byte("verify=1")) {
		t.Errorf("409 body does not tell the operator how to find out where they stand: %s", body)
	}
	// Nothing may have been copied on the refused call.
	for _, p := range orderFixtures {
		if sinkHas(t, sink, p) {
			t.Errorf("a refused cursor still copied %q", p)
		}
	}
}

// TestMigration_VerifyOnlyComparesWithoutCopying pins the closing pass
// of a chunked migration: it must answer the question and write
// nothing, both before the copy (where it reports every class missing)
// and after (where it reports verified).
func TestMigration_VerifyOnlyComparesWithoutCopying(t *testing.T) {
	src := seedSource(t, orderFixtures)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_chunk_verify")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	// Before: nothing copied, everything missing.
	code, resp, body := migrateQ(t, cfg, url.Values{"verify": {"1"}})
	if code != http.StatusOK {
		t.Fatalf("verify status %d, body %s", code, body)
	}
	if !resp.Verify {
		t.Error("verify flag is not echoed in the response")
	}
	if resp.Verified {
		t.Fatal("verified = true against an empty sink")
	}
	if resp.Total != 0 {
		t.Errorf("total_migrated = %d on a verify call, want 0 — it copied something", resp.Total)
	}
	if resp.SinkRows != 0 {
		t.Errorf("sink_rows = %d after a verify against an empty sink, want 0", resp.SinkRows)
	}
	if len(resp.MissingByCollection) != 4 {
		t.Errorf("missing_by_collection has %d entries, want 4 (every class the fixtures cover): %+v", len(resp.MissingByCollection), resp.MissingByCollection)
	}
	for _, p := range orderFixtures {
		if sinkHas(t, sink, p) {
			t.Fatalf("verify=1 copied %q", p)
		}
	}

	// After a full copy: verified, still copying nothing.
	if code, _, body := migrateQ(t, cfg, nil); code != http.StatusOK {
		t.Fatalf("full migrate status %d, body %s", code, body)
	}
	code, resp, body = migrateQ(t, cfg, url.Values{"verify": {"1"}})
	if code != http.StatusOK {
		t.Fatalf("verify-after status %d, body %s", code, body)
	}
	if !resp.Verified {
		t.Fatalf("verified = false after a full migration (missing=%+v)", resp.MissingByCollection)
	}
	if resp.Total != 0 {
		t.Errorf("total_migrated = %d on the closing verify call, want 0", resp.Total)
	}
	if len(resp.MissingByCollection) != 0 {
		t.Errorf("missing_by_collection is non-empty after a full migration: %+v", resp.MissingByCollection)
	}
	if resp.SinkRows != int64(len(orderFixtures)) {
		t.Errorf("sink_rows = %d, want %d", resp.SinkRows, len(orderFixtures))
	}
}

// TestMigration_BadParams — the two input shapes that would otherwise
// be interpreted as something the caller did not mean. `dry_run` plus
// `verify` is contradictory rather than redundant (one walks the loop
// without I/O, the other skips the loop), and a `limit` that fails to
// parse must not silently become "no limit", which is the unbounded run
// chunking exists to avoid.
func TestMigration_BadParams(t *testing.T) {
	src := seedSource(t, orderFixtures)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_chunk_badparams")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	for _, tc := range []struct {
		name string
		q    url.Values
	}{
		{"dry_run+verify", url.Values{"dry_run": {"1"}, "verify": {"1"}}},
		{"limit=abc", url.Values{"limit": {"abc"}}},
		{"limit=-1", url.Values{"limit": {"-1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, body := migrateQ(t, cfg, tc.q)
			if code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 — body %s", code, body)
			}
		})
	}
	// None of the refused calls may have written.
	for _, p := range orderFixtures {
		if sinkHas(t, sink, p) {
			t.Errorf("a refused request copied %q", p)
		}
	}
}

// TestMigration_LimitAboveSourceCountCompletesInOne — the boundary the
// operator's loop hits on its last chunk, and the shape a caller who
// guessed a generous limit gets. Must report done + verified, and must
// not report a cursor that would make the loop ask for one more chunk.
func TestMigration_LimitAboveSourceCountCompletesInOne(t *testing.T) {
	src := seedSource(t, orderFixtures)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_chunk_bigleimit")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	code, resp, body := migrateQ(t, cfg, url.Values{"limit": {"500"}})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if !resp.Done || !resp.Verified {
		t.Fatalf("done=%v verified=%v, want both true (missing=%+v)", resp.Done, resp.Verified, resp.MissingByCollection)
	}
	if resp.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", resp.Remaining)
	}
	if resp.Total != int64(len(orderFixtures)) {
		t.Errorf("total_migrated = %d, want %d", resp.Total, len(orderFixtures))
	}
	if resp.Limit != 500 {
		t.Errorf("limit = %d in the response, want the 500 that was requested", resp.Limit)
	}
}
