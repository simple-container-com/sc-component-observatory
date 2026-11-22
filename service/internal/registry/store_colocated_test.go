// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package registry

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
)

func colocated(t *testing.T) (*ColocatedStore, *storage.InMemoryStore) {
	t.Helper()
	objs := storage.NewInMemoryStore()
	return NewColocatedStore(objs, "obs"), objs
}

// TestColocated_RoundTrip — a record survives a write/read cycle with
// every field intact, including the nested telemetry sources.
func TestColocated_RoundTrip(t *testing.T) {
	ctx := context.Background()
	store, _ := colocated(t)

	want := awsRecord("forge-storage--staging", "2026.11.18-5038c3d")
	want.Service, want.Env = SplitStack(want.Stack)
	want.FirstSeenAt = t0
	want.LastSeenAt = t0
	want.DeployedAt = t0
	want.EndpointURL = "https://storage.example.com"

	if err := store.Put(ctx, want); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, found, err := store.Get(ctx, want.Stack)
	if err != nil || !found {
		t.Fatalf("get: err=%v found=%v", err, found)
	}
	if got.Stack != want.Stack || got.Version != want.Version || got.EndpointURL != want.EndpointURL {
		t.Errorf("scalar fields differ: %+v", got)
	}
	if len(got.Sources) != 1 || got.Sources[0].Ref != want.Sources[0].Ref {
		t.Errorf("sources lost in round-trip: %+v", got.Sources)
	}
	if got.Sources[0].Dims["FunctionName"] != want.Stack {
		t.Errorf("dims lost in round-trip: %+v", got.Sources[0].Dims)
	}
	if !got.DeployedAt.Equal(t0) {
		t.Errorf("DeployedAt not preserved: %v", got.DeployedAt)
	}
}

// TestColocated_GetMissingIsNotAnError — a missing service must be
// distinguishable from a broken bucket: the first is a 404, the second
// a 500, and conflating them sends an operator hunting the wrong fault.
func TestColocated_GetMissingIsNotAnError(t *testing.T) {
	store, _ := colocated(t)
	rec, found, err := store.Get(context.Background(), "never--registered")
	if err != nil {
		t.Fatalf("a missing object must not be an error, got %v", err)
	}
	if found {
		t.Errorf("found a record that was never written: %+v", rec)
	}
}

// TestColocated_ListSkipsCorruptObjects — one unreadable object must
// not blank the whole dashboard.
func TestColocated_ListSkipsCorruptObjects(t *testing.T) {
	ctx := context.Background()
	store, objs := colocated(t)

	for _, s := range []string{"a--staging", "b--staging"} {
		if err := store.Put(ctx, awsRecord(s, "v1")); err != nil {
			t.Fatalf("put %s: %v", s, err)
		}
	}
	// Hand-write a corrupt object into the services prefix.
	if _, err := objs.Put(ctx, "obs/_meta/services/corrupt--staging.json", strings.NewReader("{not json")); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}

	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list must survive a corrupt object, got %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want the 2 good records, got %d: %+v", len(got), got)
	}
}

// TestColocated_ListIsSorted — stable ordering, so the dashboard does
// not reshuffle between refreshes.
func TestColocated_ListIsSorted(t *testing.T) {
	ctx := context.Background()
	store, _ := colocated(t)
	for _, s := range []string{"zebra--staging", "alpha--prod", "alpha--staging"} {
		if err := store.Put(ctx, awsRecord(s, "v1")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"alpha--prod", "alpha--staging", "zebra--staging"}
	for i, w := range want {
		if got[i].Stack != w {
			t.Errorf("position %d: got %q want %q", i, got[i].Stack, w)
		}
	}
}

// TestColocated_StackNameWithSlashIsEscaped — a stack name containing
// a path separator must not escape its key namespace and land somewhere
// unexpected in the bucket.
func TestColocated_StackNameWithSlashIsEscaped(t *testing.T) {
	ctx := context.Background()
	store, objs := colocated(t)
	weird := "team/svc--staging"
	if err := store.Put(ctx, awsRecord(weird, "v1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, found, err := store.Get(ctx, weird)
	if err != nil || !found {
		t.Fatalf("escaped key did not round-trip: err=%v found=%v", err, found)
	}
	if got.Stack != weird {
		t.Errorf("stack mangled: %q", got.Stack)
	}
	// Exactly one object, and it must sit directly under services/.
	all, err := objs.List(ctx, "obs/_meta/services/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 object, got %d: %+v", len(all), all)
	}
}

// TestColocated_DeployHistoryNewestFirst — and idempotent on retry,
// since the event key IS the event id.
func TestColocated_DeployHistoryNewestFirst(t *testing.T) {
	ctx := context.Background()
	store, _ := colocated(t)

	for i, v := range []string{"v1", "v2", "v3"} {
		at := t0.Add(time.Duration(i) * time.Hour)
		ev := DeployEvent{
			ID: DeployEventID("svc--staging", v, at), Stack: "svc--staging",
			Version: v, DeployedAt: at,
		}
		// Append twice — a retried registration must not duplicate.
		for range 2 {
			if err := store.AppendDeployEvent(ctx, ev); err != nil {
				t.Fatalf("append %s: %v", v, err)
			}
		}
	}

	hist, err := store.DeployHistory(ctx, "svc--staging", 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("want 3 events (retries deduped by key), got %d", len(hist))
	}
	if hist[0].Version != "v3" || hist[2].Version != "v1" {
		t.Errorf("not newest-first: %v %v %v", hist[0].Version, hist[1].Version, hist[2].Version)
	}
}

// TestColocated_DeployHistoryLimit
func TestColocated_DeployHistoryLimit(t *testing.T) {
	ctx := context.Background()
	store, _ := colocated(t)
	for i := range 5 {
		at := t0.Add(time.Duration(i) * time.Hour)
		if err := store.AppendDeployEvent(ctx, DeployEvent{
			ID: DeployEventID("svc--staging", "v", at), Stack: "svc--staging", DeployedAt: at,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	hist, err := store.DeployHistory(ctx, "svc--staging", 2)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 2 {
		t.Errorf("limit ignored: got %d", len(hist))
	}
}

// TestColocated_DeleteRemovesHistory — explicit removal is the
// operator's only clean exit (no consumer-destroyed event exists), so
// it must not leave orphaned timeline objects paying storage forever.
func TestColocated_DeleteRemovesHistory(t *testing.T) {
	ctx := context.Background()
	store, objs := colocated(t)

	if _, err := Register(ctx, store, awsRecord("gone--staging", "v1"), t0); err != nil {
		t.Fatalf("register: %v", err)
	}
	if hist, _ := store.DeployHistory(ctx, "gone--staging", 0); len(hist) != 1 {
		t.Fatalf("precondition: want 1 event, got %d", len(hist))
	}

	if err := store.Delete(ctx, "gone--staging"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := store.Get(ctx, "gone--staging"); found {
		t.Error("record survived delete")
	}
	if hist, _ := store.DeployHistory(ctx, "gone--staging", 0); len(hist) != 0 {
		t.Errorf("history survived delete: %d", len(hist))
	}
	// Nothing left in the bucket under either prefix.
	for _, p := range []string{"obs/_meta/services/", "obs/_meta/deploys/"} {
		left, _ := objs.List(ctx, p)
		if len(left) != 0 {
			t.Errorf("objects left under %s: %+v", p, left)
		}
	}
}

// TestColocated_DeleteMissingIsIdempotent
func TestColocated_DeleteMissingIsIdempotent(t *testing.T) {
	store, _ := colocated(t)
	if err := store.Delete(context.Background(), "never--there"); err != nil {
		t.Errorf("delete of a missing service should be a no-op, got %v", err)
	}
}

// TestColocated_RegisterMergeRulesHold — the merge rules are the whole
// point of Register, so assert them against the REAL store and not only
// the in-memory one: a backend whose Get returns a zero value instead
// of found=false would silently defeat them.
func TestColocated_RegisterMergeRulesHold(t *testing.T) {
	ctx := context.Background()
	store, _ := colocated(t)

	if _, err := Register(ctx, store, awsRecord("svc--staging", "v1"), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	blind := ServiceRecord{Stack: "svc--staging", Cloud: CloudAWS, Template: "lambda", Version: "v2"}
	got, err := Register(ctx, store, blind, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(got.Sources) != 1 {
		t.Error("empty Sources wiped a live record through the colocated store")
	}
	if !got.FirstSeenAt.Equal(t0) {
		t.Errorf("FirstSeenAt reset: %v", got.FirstSeenAt)
	}
}

// TestColocated_DeployConfigRoundTripAndPrefixIsolation — the snapshot
// path against the REAL key layout.
//
// The prefix isolation is the load-bearing part: DeployHistory LISTs
// the deploys prefix and decodes EVERY object it finds (up to
// maxEventScan) to order the timeline. A snapshot stored under that
// same prefix would be decoded on every timeline read — a compose block
// per row, for a read that only needs ids and timestamps — and would
// also masquerade as an event with empty fields.
func TestColocated_DeployConfigRoundTripAndPrefixIsolation(t *testing.T) {
	ctx := context.Background()
	store, objs := colocated(t)

	rec := awsRecord("svc--staging", "v1")
	rec.ClientConfig = map[string]any{
		"Config": map[string]any{"config": map[string]any{"maxMemory": 512}},
	}
	if _, err := Register(ctx, store, rec, t0); err != nil {
		t.Fatalf("register: %v", err)
	}

	hist, err := store.DeployHistory(ctx, "svc--staging", 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("want 1 event, got %d — a snapshot may be masquerading as one: %+v", len(hist), hist)
	}
	if !hist[0].ConfigCaptured {
		t.Fatal("ConfigCaptured not set through the colocated store")
	}

	cfg, found, err := store.DeployConfig(ctx, "svc--staging", hist[0].ID)
	if err != nil || !found {
		t.Fatalf("snapshot: found=%v err=%v", found, err)
	}
	outer, _ := cfg["Config"].(map[string]any)
	inner, _ := outer["config"].(map[string]any)
	if fmt.Sprint(inner["maxMemory"]) != "512" {
		t.Errorf("snapshot did not survive the round-trip: %+v", cfg)
	}

	// Exactly one object under each prefix, and they are different
	// prefixes.
	for prefix, want := range map[string]int{
		"obs/_meta/deploys/":        1,
		"obs/_meta/deploy-configs/": 1,
	} {
		got, _ := objs.List(ctx, prefix)
		if len(got) != want {
			t.Errorf("%s holds %d objects, want %d: %+v", prefix, len(got), want, got)
		}
	}
}

// TestColocated_DeleteSweepsConfigSnapshots — a prefix added to the
// store but forgotten in Delete leaves objects behind forever: the
// bucket keeps paying for them and a re-registered stack inherits the
// previous incarnation's diffs.
func TestColocated_DeleteSweepsConfigSnapshots(t *testing.T) {
	ctx := context.Background()
	store, objs := colocated(t)

	rec := awsRecord("gone--staging", "v1")
	rec.ClientConfig = map[string]any{"template": "lambda"}
	if _, err := Register(ctx, store, rec, t0); err != nil {
		t.Fatalf("register: %v", err)
	}
	if left, _ := objs.List(ctx, "obs/_meta/deploy-configs/"); len(left) != 1 {
		t.Fatalf("precondition: %d snapshots", len(left))
	}

	if err := store.Delete(ctx, "gone--staging"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, p := range []string{
		"obs/_meta/services/", "obs/_meta/deploys/", "obs/_meta/deploy-configs/",
	} {
		if left, _ := objs.List(ctx, p); len(left) != 0 {
			t.Errorf("objects left under %s: %+v", p, left)
		}
	}
}
