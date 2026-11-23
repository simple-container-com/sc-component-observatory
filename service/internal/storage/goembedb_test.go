// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/FerretDB/FerretDB/ferretdb"

	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
)

// embeddedMongo spins up a fresh ferretdb instance under a temp dir
// and returns a connected mongo client. Teardown is via the returned
// cleanup — defer that from the test to tear down the listener + the
// sqlite files.
//
// No replication: these tests exercise the storage.Store contract, not
// the active-active layer. Single-node collapses w=majority to local
// commit, and the test is byte-identical to a long-lived-process embed.
func embeddedMongo(t *testing.T) (*mongo.Client, func()) {
	t.Helper()
	dir := t.TempDir()
	sqliteDir := filepath.Join(dir, "sqlite")
	if err := os.MkdirAll(sqliteDir, 0o755); err != nil {
		t.Fatalf("mkdir sqlite: %v", err)
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
			t.Logf("ferretdb.Run exited: %v", err)
		}
	}()

	uri := fdb.MongoDBURI()
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri).SetDirect(true))
	if err != nil {
		cancel()
		<-runDone
		t.Fatalf("mongo.Connect: %v", err)
	}
	if err := client.Ping(cctx, readpref.Primary()); err != nil {
		cancel()
		<-runDone
		t.Fatalf("mongo ping: %v", err)
	}
	cleanup := func() {
		_ = client.Disconnect(context.Background())
		cancel()
		<-runDone
	}
	return client, cleanup
}

func TestMongoStore_RoundTrip(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	st := storage.NewMongoStore(client).WithDatabase(uniqueDB(t))

	ctx := context.Background()
	payload := []byte(`{"stack":"forge-notifier--staging","version":"2026.11.22-abc"}`)
	// Path shape the colocated registry emits for a ServiceRecord.
	path := "_meta/services/forge-notifier--staging.json"

	n, err := st.Put(ctx, path, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("Put returned size %d, want %d", n, len(payload))
	}

	rc, err := st.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("Get read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Get payload mismatch: got %q, want %q", got, payload)
	}

	// Overwrite — ReplaceOne-with-upsert, so a second Put must land as
	// the same _id, not a duplicate.
	payload2 := []byte(`{"stack":"forge-notifier--staging","version":"2026.11.23-def"}`)
	if _, err := st.Put(ctx, path, bytes.NewReader(payload2)); err != nil {
		t.Fatalf("Put (overwrite): %v", err)
	}
	rc, err = st.Get(ctx, path)
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload2) {
		t.Fatalf("Get after overwrite: got %q, want %q", got, payload2)
	}
}

func TestMongoStore_GetMissing(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	st := storage.NewMongoStore(client).WithDatabase(uniqueDB(t))

	_, err := st.Get(context.Background(), "_meta/services/nope.json")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get missing: got %v, want ErrNotFound", err)
	}
}

func TestMongoStore_List(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	st := storage.NewMongoStore(client).WithDatabase(uniqueDB(t))
	ctx := context.Background()

	// Fixture: three services, two deploys, one health probe. Each
	// class routes to its own collection, and List against a class
	// prefix must see exactly what belongs there.
	keys := map[string][]byte{
		"_meta/services/alpha.json":               []byte(`{"stack":"alpha"}`),
		"_meta/services/beta.json":                []byte(`{"stack":"beta"}`),
		"_meta/services/gamma.json":               []byte(`{"stack":"gamma"}`),
		"_meta/deploys/alpha/aaa.json":            []byte(`{"id":"aaa"}`),
		"_meta/deploys/alpha/bbb.json":            []byte(`{"id":"bbb"}`),
		"_meta/health/alpha/2026-11-23T10_r.json": []byte(`{"status":"up"}`),
	}
	for k, v := range keys {
		if _, err := st.Put(ctx, k, bytes.NewReader(v)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}

	services, err := st.List(ctx, "_meta/services/")
	if err != nil {
		t.Fatalf("List services: %v", err)
	}
	if len(services) != 3 {
		t.Fatalf("List services: got %d, want 3 (%+v)", len(services), services)
	}
	for _, o := range services {
		if !strings.HasPrefix(o.Path, "_meta/services/") {
			t.Fatalf("List services leaked %q", o.Path)
		}
	}

	deploys, err := st.List(ctx, "_meta/deploys/alpha/")
	if err != nil {
		t.Fatalf("List deploys: %v", err)
	}
	if len(deploys) != 2 {
		t.Fatalf("List deploys: got %d, want 2 (%+v)", len(deploys), deploys)
	}

	health, err := st.List(ctx, "_meta/health/alpha/")
	if err != nil {
		t.Fatalf("List health: %v", err)
	}
	if len(health) != 1 {
		t.Fatalf("List health: got %d, want 1", len(health))
	}
}

func TestMongoStore_Delete(t *testing.T) {
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	st := storage.NewMongoStore(client).WithDatabase(uniqueDB(t))
	ctx := context.Background()

	path := "_meta/services/foo.json"
	if _, err := st.Put(ctx, path, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := st.Delete(ctx, path); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Delete(ctx, path); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Delete missing: got %v, want ErrNotFound", err)
	}
	if _, err := st.Get(ctx, path); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get after Delete: got %v, want ErrNotFound", err)
	}
}

func TestMongoStore_Presign(t *testing.T) {
	// No embedded Mongo needed — Presign is pure.
	st := storage.NewMongoStore(nil)
	_, err := st.Presign(context.Background(), "any", time.Minute)
	if !errors.Is(err, storage.ErrPresignUnsupported) {
		t.Fatalf("Presign: got %v, want ErrPresignUnsupported", err)
	}
}

func TestMongoStore_CollectionRouting(t *testing.T) {
	// Picks the right collection for every path shape the colocated
	// store emits + the kv fallback for anything else. Validates via
	// per-collection counts rather than reaching into unexported state.
	client, cleanup := embeddedMongo(t)
	defer cleanup()
	st := storage.NewMongoStore(client).WithDatabase(uniqueDB(t))
	ctx := context.Background()

	fixtures := map[string]string{
		"pfx/_meta/services/x.json":           storage.CollectionServices,
		"_meta/services/y.json":               storage.CollectionServices,
		"_meta/deploys/alpha/aaa.json":        storage.CollectionDeploys,
		"_meta/deploy-configs/alpha/bbb.json": storage.CollectionDeployConfigs,
		"_meta/health/alpha/t.json":           storage.CollectionHealth,
		"random/other/path.json":              storage.CollectionKV,
	}
	for path := range fixtures {
		if _, err := st.Put(ctx, path, bytes.NewReader([]byte("v"))); err != nil {
			t.Fatalf("Put %s: %v", path, err)
		}
	}

	counts, err := st.CollectionCounts(ctx)
	if err != nil {
		t.Fatalf("CollectionCounts: %v", err)
	}
	want := map[string]int64{
		storage.CollectionServices:      2,
		storage.CollectionDeploys:       1,
		storage.CollectionDeployConfigs: 1,
		storage.CollectionHealth:        1,
		storage.CollectionKV:            1,
	}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("collection %s: got %d, want %d (all: %+v)", k, counts[k], v, counts)
		}
	}
}

// uniqueDB picks a per-test Mongo database so tests sharing one
// embed never collide.
func uniqueDB(t *testing.T) string {
	t.Helper()
	// Mongo database names forbid ".", "$", "/", "\", "\0", " ", >64 chars.
	n := strings.ReplaceAll(t.Name(), "/", "_")
	if len(n) > 60 {
		n = n[:60]
	}
	return "obs_" + n
}
