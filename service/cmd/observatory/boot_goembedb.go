// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package main — go-embedb boot helper. Kept in its own file so the
// S3Store path (the default) remains readable without the Mongo /
// ferretdb dependency showing in main.go.
//
// Pattern mirrors go-embedb's cmd/smoke-lambda/main.go bootEmbed(): one
// cold-start boot, a package-scoped FerretDB instance plus a connected
// mongo.Client, every subsequent handler call reuses both. The Lambda-
// specific knobs (AckWaitLiveOnly, large HeartbeatTTL, auto replicaId
// from AWS_LAMBDA_LOG_STREAM_NAME) are hard-wired on when
// AWS_LAMBDA_FUNCTION_NAME is set, so an operator who enables
// USE_GOEMBEDB does not have to remember Bug 13.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/FerretDB/FerretDB/ferretdb"

	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
)

// goEmbed is the booted state. Accessed through bootGoEmbed below;
// guards cold-start re-entry with a sync.Once, same shape smoke-lambda
// uses — a Lambda container may enter the handler many times, and
// re-opening the SQLite listener on each call would turn a sub-ms read
// into an expensive connect.
type goEmbed struct {
	FDB     *ferretdb.FerretDB
	Client  *mongo.Client
	ReplSet string
	// cancel stops the FerretDB.Run goroutine. Only reached from a
	// test / graceful-shutdown path; a Lambda container simply freezes.
	cancel context.CancelFunc
	// runDone is closed when FerretDB.Run returns, so a test can wait
	// for the listener to actually be down before tearing a temp dir.
	runDone chan struct{}
}

var (
	goEmbedOnce sync.Once
	goEmbedVal  *goEmbed
	goEmbedErr  error
)

// bootGoEmbed brings up go-embedb + the Mongo client once per process.
// Re-entry after a successful boot is a no-op; a failed boot is cached
// so every subsequent call fails fast rather than retrying a listener
// on an already-bound socket (which would print a cryptic "address
// already in use" on every request).
func bootGoEmbed(logger *slog.Logger) (*goEmbed, error) {
	goEmbedOnce.Do(func() {
		goEmbedVal, goEmbedErr = bootGoEmbedOnce(logger)
	})
	return goEmbedVal, goEmbedErr
}

func bootGoEmbedOnce(logger *slog.Logger) (*goEmbed, error) {
	dir := envOrDefault("GOEMBEDB_SQLITE_DIR", "/tmp/goembedb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("goembedb: mkdir sqlite dir %q: %w", dir, err)
	}
	replSet := envOrDefault("GOEMBEDB_REPLSET_NAME", "observatory")

	cfg := &ferretdb.Config{
		Logger:      logger.With("subsystem", "ferretdb"),
		Handler:     "sqlite",
		SQLiteURL:   "file:" + dir + "/",
		Listener:    ferretdb.ListenerConfig{TCP: "127.0.0.1:0"},
		ReplSetName: replSet,
	}

	// Replication substrate. OPTIONAL on long-lived hosts (ECS / VM /
	// local smoke) — a single-node embed is a valid "just put it in
	// sqlite" deploy because the one writer keeps state across requests.
	//
	// HARD-REQUIRED on Lambda. Lambda's `/tmp` is per-container and dies
	// with every container recycle, so "no bucket" means every warm
	// container runs its own ephemeral store. For observatory specifically
	// that **resets up/down edge state across cold starts, producing
	// spurious edges** — the exact thing this component's alerting
	// exists to detect.
	//
	// So on Lambda, refuse to boot rather than warn. Operator either
	// sets GOEMBEDB_S3_BUCKET (step 2 of the cutover) or leaves
	// USE_GOEMBEDB=0; there is no silent middle ground.
	if bucket := os.Getenv("GOEMBEDB_S3_BUCKET"); bucket != "" {
		region := envOrDefault("GOEMBEDB_S3_REGION", os.Getenv("AWS_REGION"))
		rep := &ferretdb.ReplicationStorageConfig{
			Backend:  "s3",
			S3Bucket: bucket,
			S3Prefix: goEmbedS3Prefix(),
			S3Region: region,
		}
		if max := envIntOrZero("GOEMBEDB_MAX_REPLICAS"); max > 0 {
			rep.MaxReplicas = max
		}
		if ttl := envDurationOrZero("GOEMBEDB_HEARTBEAT_TTL"); ttl > 0 {
			rep.HeartbeatTTL = ttl
		}
		// Lambda-correctness defaults. Explicit env overrides win so a
		// long-lived deploy (ECS / VM) can turn them off, but the common
		// case — observatory IS a Lambda — gets Bug 13 right without the
		// operator remembering it.
		if isLambda() {
			if rep.HeartbeatTTL == 0 {
				rep.HeartbeatTTL = 3 * time.Minute
			}
			if rep.MaxReplicas == 0 {
				rep.MaxReplicas = 10
			}
			rep.AckWaitLiveOnly = true
		}
		// Explicit override still wins (useful for a non-Lambda deploy
		// that nonetheless wants live-only — e.g. a smoke).
		if envBool("GOEMBEDB_ACK_WAIT_LIVE_ONLY") {
			rep.AckWaitLiveOnly = true
		}
		cfg.ReplicationStorage = rep
		logger.Info("goembedb replication configured",
			"bucket", rep.S3Bucket, "prefix", rep.S3Prefix, "region", rep.S3Region,
			"ackWaitLiveOnly", rep.AckWaitLiveOnly, "heartbeatTTL", rep.HeartbeatTTL)
	} else if isLambda() {
		return nil, fmt.Errorf("goembedb: GOEMBEDB_S3_BUCKET is required on Lambda — /tmp is per-container and ephemeral, which resets observatory's up/down edge state across container recycles and produces spurious alerts. Set GOEMBEDB_S3_BUCKET (step 2 of the cutover) or leave USE_GOEMBEDB=0")
	} else {
		logger.Warn("GOEMBEDB_S3_BUCKET unset — running single-node, no replication")
	}

	fdb, err := ferretdb.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("goembedb: ferretdb.New: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		if runErr := fdb.Run(ctx); runErr != nil && !errors.Is(runErr, context.Canceled) {
			logger.Error("ferretdb.Run exited", "err", runErr)
		}
	}()

	uri := fdb.MongoDBURI()
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	// SetDirect: the embed advertises a single-host rs which a non-
	// direct mongo-driver waits to elect a primary against; direct
	// connection skips SDAM and pins to the loopback socket. Same
	// reasoning as smoke-lambda/main.go:229.
	client, err := mongo.Connect(cctx, options.Client().ApplyURI(uri).SetDirect(true))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("goembedb: mongo.Connect: %w", err)
	}
	if err := client.Ping(cctx, readpref.Primary()); err != nil {
		cancel()
		return nil, fmt.Errorf("goembedb: mongo ping: %w", err)
	}

	logger.Info("goembedb booted",
		"uri", uri, "replSet", replSet, "sqliteDir", dir,
		"replicationEnabled", cfg.ReplicationStorage != nil)

	return &goEmbed{
		FDB:     fdb,
		Client:  client,
		ReplSet: replSet,
		cancel:  cancel,
		runDone: runDone,
	}, nil
}

// goEmbedS3Prefix is the ONE place the replication prefix is resolved.
// Both the boot path (which hands it to ferretdb) and the migration
// endpoint (which must exclude it from the source listing) call this,
// so an operator who overrides GOEMBEDB_S3_PREFIX cannot move the
// replication records out from under the exclusion.
func goEmbedS3Prefix() string {
	return envOrDefault("GOEMBEDB_S3_PREFIX", storage.DefaultGoEmbedS3Prefix)
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envIntOrZero(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func envDurationOrZero(name string) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0
	}
	return d
}

func envBool(name string) bool {
	switch os.Getenv(name) {
	case "1", "true", "TRUE", "yes", "on":
		return true
	}
	return false
}

func isLambda() bool {
	return os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != ""
}
