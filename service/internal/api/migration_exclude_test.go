// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/simple-container-com/sc-component-observatory/service/internal/api"
	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
)

// replicationFixtures are keys of the shape go-embedb's active-active
// replication writes under GOEMBEDB_S3_PREFIX, inside the SAME bucket
// the migration reads from (the fleet points GOEMBEDB_S3_BUCKET at the
// bucket the component already owns, on purpose).
//
// `settings/retention.json` is NOT one of them, and it is the most
// important key in this fixture: it is a legitimate non-`_meta/` key,
// so it classifies as `kv` exactly like a replication record does. A
// fix that excluded the whole `kv` CLASS instead of the replication
// PREFIX would pass every other assertion in this file and silently
// stop migrating real data. Keep it here.
var replicationFixtures = []string{
	storage.DefaultGoEmbedS3Prefix + "replicas/r1/heartbeat.json",
	storage.DefaultGoEmbedS3Prefix + "wal/000000000001.log",
	storage.DefaultGoEmbedS3Prefix + "wal/000000000002.log",
	"settings/retention.json",
}

// TestMigration_ExcludesSinkOwnReplicationKeys is the regression test
// for the migration reading the sink's write-ahead log as pending work.
//
// Live-caught 2026-11-24 during the observatory wet run: `kv` went from
// 3 source keys at dry-run to 9166 eighteen minutes later — a 1:1 echo
// of the ~9127 rows the migration had just written — and `verified`
// became a false negative that could never go true, because copying is
// what produced the shortfall. See excludeReplicationKeys for the
// three individually-safe decisions that composed into it.
func TestMigration_ExcludesSinkOwnReplicationKeys(t *testing.T) {
	src := seedSource(t, append(append([]string{}, orderFixtures...), replicationFixtures...))
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_exclude_repl")
	cfg := &api.MigrationConfig{
		Source:        src,
		Sink:          sink,
		AdminToken:    "secret-token",
		ExcludePrefix: storage.DefaultGoEmbedS3Prefix,
	}

	code, resp, body := migrateQ(t, cfg, url.Values{})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}

	// In scope: the five _meta/ fixtures plus the one real kv key. The
	// three replication records are not.
	wantInScope := len(orderFixtures) + 1
	if resp.S3Count != wantInScope {
		t.Errorf("s3_count = %d, want %d — s3_count is the IN-SCOPE count, so replication keys must not be in it", resp.S3Count, wantInScope)
	}
	if resp.ExcludedReplication != 3 {
		t.Errorf("excluded_replication = %d, want 3 — the count must be reported, not silently subtracted from s3_count", resp.ExcludedReplication)
	}

	// The real kv key still migrates. This is the assertion that fails
	// if someone "fixes" this by dropping the kv class.
	if got := resp.SourceByCollection[storage.CollectionKV]; got != 1 {
		t.Errorf("source_by_collection[kv] = %d, want 1 (settings/retention.json) — the exclusion is by PREFIX, not by class", got)
	}
	if !sinkHas(t, sink, "settings/retention.json") {
		t.Error("the sink is missing settings/retention.json: a legitimate non-_meta key was excluded along with the replication records")
	}

	// And the replication records did not get copied into the sink,
	// where each copy would have produced another replication record.
	for _, p := range replicationFixtures[:3] {
		if sinkHas(t, sink, p) {
			t.Errorf("sink holds %q — a replication record was migrated into the store that wrote it", p)
		}
	}

	if !resp.Verified {
		t.Errorf("verified = false after a complete run; missing_by_collection = %+v", resp.MissingByCollection)
	}
}

// TestMigration_WithoutExcludePrefixTheReplicationKeysAreInput is the
// inversion, kept as a test rather than performed by hand once.
//
// It pins the pre-fix behaviour as the thing that changed: the same
// source, the same handler, ExcludePrefix empty, and the replication
// records are enumerated and copied. That matters for two reasons —
// it proves the earlier test is not passing for some unrelated reason,
// and an empty ExcludePrefix is a legitimate configuration (a source
// bucket that is not also the replication target), so this is the
// documented behaviour there, not merely a bug being recorded.
func TestMigration_WithoutExcludePrefixTheReplicationKeysAreInput(t *testing.T) {
	src := seedSource(t, append(append([]string{}, orderFixtures...), replicationFixtures...))
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_exclude_off")
	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "secret-token"}

	code, resp, body := migrateQ(t, cfg, url.Values{})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}

	wantAll := len(orderFixtures) + len(replicationFixtures)
	if resp.S3Count != wantAll {
		t.Errorf("s3_count = %d, want %d", resp.S3Count, wantAll)
	}
	if resp.ExcludedReplication != 0 {
		t.Errorf("excluded_replication = %d, want 0 with an empty ExcludePrefix", resp.ExcludedReplication)
	}
	// All four non-_meta keys land in kv — which is how a replication
	// record became indistinguishable from real data in the first place.
	if got := resp.SourceByCollection[storage.CollectionKV]; got != 4 {
		t.Errorf("source_by_collection[kv] = %d, want 4", got)
	}
	if !sinkHas(t, sink, storage.DefaultGoEmbedS3Prefix+"wal/000000000001.log") {
		t.Error("expected the unfiltered run to copy a replication record; if it did not, this test is no longer the inversion of the one above")
	}
}

// TestMigration_ExcludePrefixIsAnchored — the prefix is a prefix, not a
// substring. A key that merely CONTAINS the replication prefix further
// down its path is real data and must migrate.
func TestMigration_ExcludePrefixIsAnchored(t *testing.T) {
	keys := []string{
		"_meta/services/alpha.json",
		"backups/" + storage.DefaultGoEmbedS3Prefix + "notes.json",
	}
	src := seedSource(t, keys)
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_exclude_anchor")
	cfg := &api.MigrationConfig{
		Source:        src,
		Sink:          sink,
		AdminToken:    "secret-token",
		ExcludePrefix: storage.DefaultGoEmbedS3Prefix,
	}

	code, resp, body := migrateQ(t, cfg, url.Values{})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	if resp.ExcludedReplication != 0 {
		t.Errorf("excluded_replication = %d, want 0 — the prefix match is anchored at the start of the key", resp.ExcludedReplication)
	}
	if !sinkHas(t, sink, keys[1]) {
		t.Errorf("sink is missing %q: the exclusion matched mid-path", keys[1])
	}
}
