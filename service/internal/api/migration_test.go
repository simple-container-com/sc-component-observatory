// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/FerretDB/FerretDB/ferretdb"

	"github.com/simple-container-com/sc-component-observatory/service/internal/api"
	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
)

// TestMigration_HappyPath puts 10 records in an InMemoryStore acting
// as S3, runs the migration against a fresh embedded Mongo, and
// verifies the count parity the response reports back.
func TestMigration_HappyPath(t *testing.T) {
	src := storage.NewInMemoryStore()
	fixtures := []string{
		"_meta/services/alpha.json",
		"_meta/services/beta.json",
		"_meta/services/gamma.json",
		"_meta/deploys/alpha/d1.json",
		"_meta/deploys/alpha/d2.json",
		"_meta/deploys/beta/d3.json",
		"_meta/deploy-configs/alpha/d1.json",
		"_meta/health/alpha/t1.json",
		"_meta/health/beta/t2.json",
		"random/other.json",
	}
	for _, p := range fixtures {
		body := fmt.Sprintf(`{"path":%q}`, p)
		if _, err := src.Put(context.Background(), p, bytes.NewReader([]byte(body))); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}

	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_migration_happy")

	cfg := &api.MigrationConfig{
		Source:     src,
		Sink:       sink,
		AdminToken: "secret-token",
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-s3-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "secret-token")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Migrate: status %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		DryRun   bool             `json:"dry_run"`
		S3Count  int              `json:"s3_count"`
		Migrated map[string]int64 `json:"migrated"`
		Total    int64            `json:"total_migrated"`
		Verified bool             `json:"verified"`
		Errors   []any            `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, w.Body.String())
	}
	if resp.DryRun {
		t.Fatalf("Migrate: dry_run unexpectedly true")
	}
	if resp.S3Count != len(fixtures) {
		t.Fatalf("Migrate: s3_count = %d, want %d", resp.S3Count, len(fixtures))
	}
	if resp.Total != int64(len(fixtures)) {
		t.Fatalf("Migrate: total_migrated = %d, want %d (errors=%v)", resp.Total, len(fixtures), resp.Errors)
	}
	if !resp.Verified {
		t.Fatalf("Migrate: verified = false (errors=%v, migrated=%+v)", resp.Errors, resp.Migrated)
	}

	// Validate per-class routing landed each fixture in the right
	// collection.
	want := map[string]int64{
		storage.CollectionServices:      3,
		storage.CollectionDeploys:       3,
		storage.CollectionDeployConfigs: 1,
		storage.CollectionHealth:        2,
		storage.CollectionKV:            1,
	}
	for k, v := range want {
		if resp.Migrated[k] != v {
			t.Fatalf("Migrate: migrated[%s] = %d, want %d (all: %+v)", k, resp.Migrated[k], v, resp.Migrated)
		}
	}

	// Re-run must be idempotent (upsert semantics) — total stays the same.
	req2 := httptest.NewRequest(http.MethodPost, "/admin/migrate-s3-to-goembedb", nil)
	req2.Header.Set("X-Admin-Token", "secret-token")
	w2 := httptest.NewRecorder()
	cfg.Migrate(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("Migrate re-run: status %d", w2.Code)
	}
	var resp2 struct {
		Migrated map[string]int64 `json:"migrated"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	for k, v := range want {
		if resp2.Migrated[k] != v {
			t.Fatalf("Migrate re-run: migrated[%s] = %d, want %d (not idempotent)", k, resp2.Migrated[k], v)
		}
	}

	// Round-trip the body of one record to confirm bytes are stored
	// verbatim (binary subtype, no JSON re-encode round-trip at the
	// storage layer).
	rc, err := sink.Get(context.Background(), "_meta/services/beta.json")
	if err != nil {
		t.Fatalf("Get after migrate: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	want1 := `{"path":"_meta/services/beta.json"}`
	if string(got) != want1 {
		t.Fatalf("Get after migrate: got %q, want %q", got, want1)
	}
}

func TestMigration_DryRun(t *testing.T) {
	src := storage.NewInMemoryStore()
	for i := 0; i < 5; i++ {
		p := fmt.Sprintf("_meta/services/svc-%d.json", i)
		if _, err := src.Put(context.Background(), p, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	client, cleanup := embeddedMongo(t)
	defer cleanup()
	sink := storage.NewMongoStore(client).WithDatabase("obs_migration_dryrun")

	cfg := &api.MigrationConfig{Source: src, Sink: sink, AdminToken: "t"}

	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-s3-to-goembedb?dry_run=1", nil)
	req.Header.Set("X-Admin-Token", "t")
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Migrate dry: status %d", w.Code)
	}
	var resp struct {
		DryRun   bool             `json:"dry_run"`
		S3Count  int              `json:"s3_count"`
		Total    int64            `json:"total_migrated"`
		Verified bool             `json:"verified"`
		Migrated map[string]int64 `json:"migrated"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.DryRun || resp.S3Count != 5 || resp.Total != 5 {
		t.Fatalf("dry-run response wrong: %+v", resp)
	}
	if resp.Verified {
		t.Fatalf("dry-run claimed verified, which must never be")
	}
	// No writes should have landed.
	if n := resp.Migrated[storage.CollectionServices]; n != 0 {
		t.Fatalf("dry-run leaked write: services count = %d, want 0", n)
	}
}

func TestMigration_AuthAndGate(t *testing.T) {
	// Endpoint disabled when AdminToken is empty.
	cfg := &api.MigrationConfig{AdminToken: ""}
	req := httptest.NewRequest(http.MethodPost, "/admin/migrate-s3-to-goembedb", nil)
	w := httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled: got %d, want 404", w.Code)
	}

	// Enabled but no token header → 401.
	cfg.AdminToken = "s"
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-s3-to-goembedb", nil)
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no header: got %d, want 401", w.Code)
	}

	// Wrong token → 401.
	req = httptest.NewRequest(http.MethodPost, "/admin/migrate-s3-to-goembedb", nil)
	req.Header.Set("X-Admin-Token", "other")
	w = httptest.NewRecorder()
	cfg.Migrate(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", w.Code)
	}
}

// embeddedMongo mirrors storage/goembedb_test.go's helper; duplicated
// here rather than exported from `storage` because the helper's
// surface is deliberately test-local (it owns a TempDir, a listener,
// and a background goroutine — exporting it would invite production
// callers).
func embeddedMongo(t *testing.T) (*mongo.Client, func()) {
	t.Helper()
	dir := t.TempDir()
	sqliteDir := filepath.Join(dir, "sqlite")
	if err := os.MkdirAll(sqliteDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := &ferretdb.Config{
		Handler:   "sqlite",
		SQLiteURL: "file:" + sqliteDir + "/",
		Listener:  ferretdb.ListenerConfig{TCP: "127.0.0.1:0"},
	}
	fdb, err := ferretdb.New(cfg)
	if err != nil {
		t.Fatalf("ferretdb.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if err := fdb.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("Run exit: %v", err)
		}
	}()
	uri := fdb.MongoDBURI()
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri).SetDirect(true))
	if err != nil {
		cancel()
		<-runDone
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Ping(cctx, readpref.Primary()); err != nil {
		cancel()
		<-runDone
		t.Fatalf("Ping: %v", err)
	}
	return client, func() {
		_ = client.Disconnect(context.Background())
		cancel()
		<-runDone
	}
}

// Keep `strings` import used so goimports doesn't prune it when tests
// shrink in a refactor.
var _ = strings.Contains
