// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// MongoStore backs the storage.Store interface with an embedded
// go-embedb (ferretdb + sqlite + active-active S3 replication) talking
// Mongo over a loopback TCP listener.
//
// This is the NEW backend introduced behind USE_GOEMBEDB=1
// (sc-component-observatory-goembedb-migration). The existing S3Store
// stays the default; the operator flips the env var once per-stack
// after a `/admin/migrate-s3-to-goembedb` dry-run verifies count parity.
//
// ## Storage shape
//
// Every storage key `<prefix>/_meta/<class>/...` becomes one document
// `{_id: "<full-path>", body: <binary>, updatedAt: <t>}` in a Mongo
// collection picked per logical class:
//
//	_meta/services/*           → collection "services"
//	_meta/deploys/*            → collection "deploys"
//	_meta/deploy-configs/*     → collection "deploy_configs"
//	_meta/health/*             → collection "health"
//	anything else              → collection "kv"
//
// One collection per class keeps List cheap: the dashboard's hot paths
// (list services, latest health per stack, deploy timeline) each hit a
// single collection with a single `^prefix` regex, instead of filtering
// an undifferentiated "objects" collection. The alternative — one
// collection, filter by `_id` prefix — would make the health prune sweep
// (which pages every health key across every stack) walk every document
// the service has ever written.
//
// ## Presign
//
// NOT implemented. `grep -rn 'storage\.Presign\|store\.Presign\|
// objects\.Presign\|\.Presign(' service` across the observatory tree
// shows only the interface definition + the three backend
// implementations; no caller invokes it. The FS/S3/mem stores retain
// Presign because the Store interface requires it and other consumers
// of this layout (sc-component-backup, sc-component-notifier) DO
// presign. Observatory's registry is JSON-over-storage; it never hands
// a signed URL to the UI or a scheduled worker.
//
// MongoStore.Presign returns ErrPresignUnsupported so a future caller
// that reaches for it fails loudly instead of silently returning an
// empty string.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrPresignUnsupported is returned by MongoStore.Presign.
//
// Separated from ErrNotFound because "cannot sign" and "does not exist"
// are different states the caller may want to distinguish.
var ErrPresignUnsupported = errors.New("storage.MongoStore: Presign is not supported on this backend")

// MongoDatabase is the Mongo database name the store writes under. A
// single database keeps bootstrap trivial (`createCollection` fires on
// first write via driver autoCreate); the collection split lives
// ONE level down.
const MongoDatabase = "observatory"

// MongoStore implements Store on top of a mongo.Client.
//
// Holds a package-selected collection router (see collectionFor). No
// background goroutines of its own — connection lifecycle is the
// caller's (see cmd/observatory/boot_goembedb.go, which owns the
// embedded ferretdb instance and the client).
type MongoStore struct {
	client *mongo.Client
	dbName string
}

// MongoDoc is the on-disk document shape. Kept small on purpose: the
// storage.Store contract is bytes-in/bytes-out, not structured docs,
// so Body is an opaque BSON binary rather than an embedded object.
//
// A structured representation would have to decode on every read and
// re-encode on every write, which is wasted work when the only caller
// (the colocated registry store) already JSON-marshals its own values
// at the layer above.
type MongoDoc struct {
	ID        string           `bson:"_id"`
	Body      primitive.Binary `bson:"body"`
	UpdatedAt time.Time        `bson:"updatedAt"`
}

// NewMongoStore builds a store around an already-connected client.
//
// The caller owns the client's lifetime; MongoStore.Close is a no-op so
// a boot helper that keeps the client for other uses (health probe,
// admin endpoints) does not have to split ownership.
func NewMongoStore(client *mongo.Client) *MongoStore {
	return &MongoStore{client: client, dbName: MongoDatabase}
}

// WithDatabase overrides the Mongo database name; useful in tests.
func (s *MongoStore) WithDatabase(name string) *MongoStore {
	cp := *s
	cp.dbName = name
	return &cp
}

// Put writes body under `path`, upserting by _id.
//
// Returns the size written. Mirrors the InMemoryStore shape: the body
// is fully read before the upsert so a broken Reader surfaces as an
// error rather than a half-written document (the colocated registry
// marshals JSON into a *bytes.Reader before calling Put, so this is
// cheap).
func (s *MongoStore) Put(ctx context.Context, path string, body io.Reader) (int64, error) {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return 0, errors.New("storage.MongoStore.Put: empty path")
	}
	buf, err := io.ReadAll(body)
	if err != nil {
		return 0, fmt.Errorf("storage.MongoStore.Put: read body: %w", err)
	}
	coll := s.collectionFor(path)
	doc := MongoDoc{
		ID:        path,
		Body:      primitive.Binary{Subtype: 0x00, Data: buf},
		UpdatedAt: time.Now().UTC(),
	}
	_, err = coll.ReplaceOne(
		ctx,
		bson.M{"_id": path},
		doc,
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		return 0, fmt.Errorf("storage.MongoStore.Put %q: %w", path, err)
	}
	return int64(len(buf)), nil
}

// Get returns a ReadCloser over the stored body. Missing key →
// ErrNotFound so the colocated registry's "object is absent vs backend
// is broken" split still works.
func (s *MongoStore) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	path = strings.TrimPrefix(path, "/")
	coll := s.collectionFor(path)
	var doc MongoDoc
	err := coll.FindOne(ctx, bson.M{"_id": path}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage.MongoStore.Get %q: %w", path, err)
	}
	return io.NopCloser(bytes.NewReader(doc.Body.Data)), nil
}

// List returns every key whose _id starts with `prefix`.
//
// Uses an anchored `^<quoted-prefix>` regex rather than the
// `{$gte, $lt}` range trick: the colocated store's List calls are all
// against stable dir-shaped prefixes (`_meta/services/`, …), so the
// regex is fast enough and the semantics match S3's prefix listing
// byte-for-byte.
//
// The projection omits the body — see listProjection. That is not an
// optimisation, it is the difference between this backend being usable
// and not: see the 10.4 s measurement in that comment.
func (s *MongoStore) List(ctx context.Context, prefix string) ([]StoredObject, error) {
	prefix = strings.TrimPrefix(prefix, "/")
	coll := s.collectionFor(prefix)
	filter := bson.M{}
	if prefix != "" {
		filter = bson.M{"_id": bson.M{"$regex": "^" + regexp.QuoteMeta(prefix)}}
	}
	opts := options.Find().SetProjection(listProjection())
	cur, err := coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("storage.MongoStore.List %q: %w", prefix, err)
	}
	defer cur.Close(ctx)
	var out []StoredObject
	for cur.Next(ctx) {
		var doc MongoDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("storage.MongoStore.List %q: decode: %w", prefix, err)
		}
		out = append(out, StoredObject{
			Path:       doc.ID,
			ModifiedAt: doc.UpdatedAt,
		})
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("storage.MongoStore.List %q: cursor: %w", prefix, err)
	}
	return out, nil
}

// listProjection is the field set List asks for, factored out so a test
// can assert what is NOT in it.
//
// `body` MUST NOT be here. A listing is keys and modified times; the
// bytes are fetched by Get, one key at a time, by the caller that
// actually wants them. Including `body` makes every List transfer the
// full payload of every matching document and then throw it away.
//
// This is not a micro-optimisation, and the cost is not proportional to
// anything a reader would expect. Live-measured 2026-11-24, immediately
// after the USE_GOEMBEDB=1 cutover: `GET /api/v1/services` took
// **10.4-11.4 s to return 8 rows** — indistinguishable from the S3
// baseline it was meant to improve on. The handler calls LatestHealth
// per service, which lists that service's whole health prefix (~1112
// keys per stack) to sort-and-take 50. With `body` in the projection
// that is ~8900 full health documents pulled over the loopback and
// BSON-decoded per request, to compute a field (the long-deleted
// StoredObject.Size) that no caller ever read.
//
// The reason it was invisible in review is worth keeping: on S3,
// ListObjectsV2 is **metadata-only** and cannot carry payloads, so the
// same code shape costs nothing there. Moving to a document store
// changed what a "list" means, and the body tagged along. The doc
// comment on List had claimed the body was omitted since the file was
// written; it was the code that was wrong, not the comment.
func listProjection() bson.M {
	return bson.M{"_id": 1, "updatedAt": 1}
}

// Delete removes one key. Missing → ErrNotFound, matching S3Store /
// FSStore so the colocated store's idempotent-delete path works the
// same way on both backends.
func (s *MongoStore) Delete(ctx context.Context, path string) error {
	path = strings.TrimPrefix(path, "/")
	coll := s.collectionFor(path)
	res, err := coll.DeleteOne(ctx, bson.M{"_id": path})
	if err != nil {
		return fmt.Errorf("storage.MongoStore.Delete %q: %w", path, err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// Presign is not implemented — see file header. Returns a typed error
// so a future caller surfaces loudly rather than seeing an empty URL.
func (s *MongoStore) Presign(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", ErrPresignUnsupported
}

// Collections is the full set of collection names MongoStore may write
// to. Enumerated so the migration endpoint can produce a count
// breakdown that the operator can diff against the S3 LIST count.
var Collections = []string{
	CollectionServices,
	CollectionDeploys,
	CollectionDeployConfigs,
	CollectionHealth,
	CollectionKV,
}

// DefaultGoEmbedS3Prefix is the key prefix go-embedb's active-active
// replication writes its own records under, inside GOEMBEDB_S3_BUCKET.
// Overridable by GOEMBEDB_S3_PREFIX; the default lives here rather than
// at the boot site because TWO callers need to agree on it, and they
// are in different packages:
//
//  1. the boot path, which hands it to ferretdb as the replication
//     prefix (cmd/observatory/boot_goembedb.go);
//  2. the migration endpoint, which must EXCLUDE it from the source
//     listing (internal/api/migration.go).
//
// They have to agree because the fleet deliberately points
// GOEMBEDB_S3_BUCKET at the bucket the component already owns — the
// same bucket the S3Store it is replacing reads and writes (see
// server.yaml's `GOEMBEDB_S3_BUCKET: sc-observatory-data--staging`,
// chosen so the cutover creates no new paid resource and needs no new
// IAM grant). That is safe for reads, because this prefix cannot
// collide with the `_meta/` tree. It is NOT safe for a migration that
// enumerates the whole bucket: see excludeReplicationKeys.
//
// If these two drift, the symptom is not an error. It is a migration
// that never reports verified.
const DefaultGoEmbedS3Prefix = "sc-component-observatory/"

// Collection names. Lower-case, underscore-separated — the Mongo
// convention and the one go-embedb's own metadata uses.
const (
	CollectionServices      = "services"
	CollectionDeploys       = "deploys"
	CollectionDeployConfigs = "deploy_configs"
	CollectionHealth        = "health"
	CollectionKV            = "kv"
)

// collectionFor routes a path to its collection.
//
// Matches on the "_meta/<class>/" segment the colocated registry writes:
//
//	[<any-prefix>/]_meta/services/*       → services
//	[<any-prefix>/]_meta/deploys/*        → deploys
//	[<any-prefix>/]_meta/deploy-configs/* → deploy_configs
//	[<any-prefix>/]_meta/health/*         → health
//	[<any-prefix>/]_meta/health-latest/*  → health
//
// Anything else falls back to `kv` so a future caller that writes a
// path outside this tree is still served, just slowly on List.
//
// The prefix the caller passed into List (e.g. `<prefix>/_meta/services/`)
// is sufficient to choose the right collection; the test suite walks
// every path shape the colocated store produces.
func (s *MongoStore) collectionFor(path string) *mongo.Collection {
	return s.client.Database(s.dbName).Collection(collectionNameFor(path))
}

// CollectionNameFor is the exported form of the path→collection router,
// for callers outside this package that need to know which collection a
// SOURCE key would land in without having a MongoStore to hand.
//
// The migration endpoint uses it to bucket an S3 listing by destination
// collection, so a partial migration is legible per class instead of
// hiding inside a single sum. That distinction is load-bearing: keys
// migrate in sorted path order, which puts `services` — the collection
// the dashboard renders — behind every `health` tick, so a copy that
// stops early produces a full-looking row count and an empty dashboard.
func CollectionNameFor(path string) string { return collectionNameFor(path) }

func collectionNameFor(path string) string {
	// Fast-path the exact substrings. order matters only in that
	// "deploy-configs/" is checked before "deploys/" — the two classes
	// share the "deploy" root.
	switch {
	case containsSegment(path, "_meta/services/"):
		return CollectionServices
	case containsSegment(path, "_meta/deploy-configs/"):
		return CollectionDeployConfigs
	case containsSegment(path, "_meta/deploys/"):
		return CollectionDeploys
	case containsSegment(path, "_meta/health/"),
		containsSegment(path, "_meta/health-latest/"):
		return CollectionHealth
	}
	// A key sitting directly at `_meta/services.json`-like paths (no
	// trailing /) is routed by the class dir name too, in case a future
	// writer uses that shape.
	switch {
	case strings.HasSuffix(path, "_meta/services") || strings.Contains(path, "_meta/services."):
		return CollectionServices
	case strings.HasSuffix(path, "_meta/deploy-configs") || strings.Contains(path, "_meta/deploy-configs."):
		return CollectionDeployConfigs
	case strings.HasSuffix(path, "_meta/deploys") || strings.Contains(path, "_meta/deploys."):
		return CollectionDeploys
	case strings.HasSuffix(path, "_meta/health") || strings.Contains(path, "_meta/health."):
		return CollectionHealth
	}
	return CollectionKV
}

// containsSegment is a small helper: `strings.Contains` with the extra
// safety that the match is anchored on a `/` on the left (or at the
// start) so `my_meta/services/` would NOT route to services.
func containsSegment(haystack, needle string) bool {
	i := strings.Index(haystack, needle)
	if i < 0 {
		return false
	}
	return i == 0 || haystack[i-1] == '/'
}

// CollectionCounts returns a per-collection document count. Used by the
// migration endpoint to produce a verification report.
func (s *MongoStore) CollectionCounts(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, name := range Collections {
		n, err := s.client.Database(s.dbName).Collection(name).CountDocuments(ctx, bson.M{})
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", name, err)
		}
		out[name] = n
	}
	return out, nil
}
