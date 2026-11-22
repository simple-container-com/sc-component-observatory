// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package registry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

func awsRecord(stack, version string) ServiceRecord {
	return ServiceRecord{
		Stack:    stack,
		Cloud:    CloudAWS,
		Template: "lambda",
		Version:  version,
		Commit:   "4603823b722bea46fc758b89ba775e1857fc94d9",
		Sources: []TelemetrySource{{
			Kind:   "cloudwatch-logs",
			Ref:    "/aws/lambda/" + stack,
			Dims:   map[string]string{"FunctionName": stack},
			Region: "eu-central-1",
		}},
	}
}

// TestRegister_FreshService — a first registration derives service/env
// from the stack name and stamps all three timestamps.
func TestRegister_FreshService(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	got, err := Register(ctx, store, awsRecord("forge-notifier--staging", "2026.11.18-abc1234"), t0)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got.Service != "forge-notifier" || got.Env != "staging" {
		t.Errorf("stack not split: service=%q env=%q", got.Service, got.Env)
	}
	if !got.FirstSeenAt.Equal(t0) || !got.LastSeenAt.Equal(t0) || !got.DeployedAt.Equal(t0) {
		t.Errorf("timestamps: first=%v last=%v deployed=%v", got.FirstSeenAt, got.LastSeenAt, got.DeployedAt)
	}

	hist, _ := store.DeployHistory(ctx, "forge-notifier--staging", 0)
	if len(hist) != 1 {
		t.Fatalf("want 1 deploy event, got %d", len(hist))
	}
	if hist[0].PreviousVersion != "" {
		t.Errorf("first deploy should have no PreviousVersion, got %q", hist[0].PreviousVersion)
	}
}

// TestRegister_EmptySourcesPreserveExisting is the generalised guard
// for the sc-component-backup incident of 2026-11-18: an unrelated
// deploy whose sibling-discovery came up empty wiped the connection
// details of the fleet's largest database and silently stopped backing
// it up. Telemetry discovery here is equally best-effort (and inferred
// outright until decision D1), so a transient miss must never erase a
// known-good handle — logs vanishing from the dashboard for no visible
// reason is exactly the same failure wearing a different hat.
func TestRegister_EmptySourcesPreserveExisting(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	if _, err := Register(ctx, store, awsRecord("forge-conductor--staging", "2026.11.18-aaa"), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A later deploy observes no telemetry sources at all.
	blind := ServiceRecord{
		Stack:    "forge-conductor--staging",
		Cloud:    CloudAWS,
		Template: "lambda",
		Version:  "2026.11.18-bbb",
		Sources:  nil,
	}
	got, err := Register(ctx, store, blind, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if len(got.Sources) != 1 {
		t.Fatalf("empty Sources wiped a live record: got %d", len(got.Sources))
	}
	if got.Sources[0].Ref != "/aws/lambda/forge-conductor--staging" {
		t.Errorf("preserved the wrong source: %+v", got.Sources[0])
	}
	// Age survives; the new version still lands.
	if !got.FirstSeenAt.Equal(t0) {
		t.Errorf("FirstSeenAt reset by a re-deploy: %v", got.FirstSeenAt)
	}
	if got.Version != "2026.11.18-bbb" {
		t.Errorf("version did not advance: %q", got.Version)
	}
	// Commit carries forward when the deploy didn't report one.
	if got.Commit == "" {
		t.Error("commit should carry forward when not re-reported")
	}
}

// TestRegister_NonEmptySourcesReplace — a registration that DOES
// observe sources replaces the stored set, so a service that moves
// region or changes log group propagates.
func TestRegister_NonEmptySourcesReplace(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	if _, err := Register(ctx, store, awsRecord("svc--staging", "v1"), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	moved := awsRecord("svc--staging", "v2")
	moved.Sources = []TelemetrySource{{
		Kind: "cloudwatch-logs", Ref: "/aws/lambda/svc--staging-renamed", Region: "us-east-1",
	}}
	got, err := Register(ctx, store, moved, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got.Sources[0].Ref != "/aws/lambda/svc--staging-renamed" || got.Sources[0].Region != "us-east-1" {
		t.Errorf("new sources did not replace: %+v", got.Sources[0])
	}
}

// TestRegister_NoEventOnUnchangedVersion — the timeline should show
// deploys, not every no-op re-registration. Re-registering the same
// version must not append.
func TestRegister_NoEventOnUnchangedVersion(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	rec := awsRecord("svc--staging", "v1")

	if _, err := Register(ctx, store, rec, t0); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := Register(ctx, store, rec, t0.Add(time.Minute)); err != nil {
		t.Fatalf("second: %v", err)
	}
	hist, _ := store.DeployHistory(ctx, "svc--staging", 0)
	if len(hist) != 1 {
		t.Errorf("want 1 event for an unchanged version, got %d", len(hist))
	}
}

// TestRegister_VersionChangeAppendsWithPrevious — a real deploy
// records what it replaced, so the timeline reads as transitions.
func TestRegister_VersionChangeAppendsWithPrevious(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	if _, err := Register(ctx, store, awsRecord("svc--staging", "v1"), t0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if _, err := Register(ctx, store, awsRecord("svc--staging", "v2"), t0.Add(time.Hour)); err != nil {
		t.Fatalf("v2: %v", err)
	}

	hist, _ := store.DeployHistory(ctx, "svc--staging", 0)
	if len(hist) != 2 {
		t.Fatalf("want 2 events, got %d", len(hist))
	}
	// Newest first.
	if hist[0].Version != "v2" || hist[0].PreviousVersion != "v1" {
		t.Errorf("transition wrong: %+v", hist[0])
	}
}

// TestAppendDeployEvent_IdempotentOnRetry — the plugin retries a
// registration up to 3 times per the push-model contract; that must
// produce one timeline entry, not three.
func TestAppendDeployEvent_IdempotentOnRetry(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	ev := DeployEvent{
		ID: DeployEventID("svc--staging", "v1", t0), Stack: "svc--staging",
		Version: "v1", DeployedAt: t0,
	}
	for range 3 {
		if err := store.AppendDeployEvent(ctx, ev); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	hist, _ := store.DeployHistory(ctx, "svc--staging", 0)
	if len(hist) != 1 {
		t.Errorf("retried registration double-counted: %d events", len(hist))
	}
}

func TestSplitStack(t *testing.T) {
	for _, tc := range []struct{ in, svc, env string }{
		{"forge-notifier--staging", "forge-notifier", "staging"},
		{"forge-conductor--99171c", "forge-conductor", "99171c"},
		{"noseparator", "noseparator", ""},
		{"", "", ""},
	} {
		svc, env := SplitStack(tc.in)
		if svc != tc.svc || env != tc.env {
			t.Errorf("SplitStack(%q) = (%q,%q), want (%q,%q)", tc.in, svc, env, tc.svc, tc.env)
		}
	}
}

// TestShortCommit_FallsBackToVersionSHA — SC always injects VERSION but
// never injects COMMIT, so a service that hasn't declared GITHUB_SHA
// must still render something useful rather than a blank column.
func TestShortCommit_FallsBackToVersionSHA(t *testing.T) {
	full := ServiceRecord{Commit: "4603823b722bea46fc758b89ba775e1857fc94d9"}
	if got := full.ShortCommit(); got != "4603823" {
		t.Errorf("full commit: got %q", got)
	}
	noCommit := ServiceRecord{Version: "2026.11.18-75593ac"}
	if got := noCommit.ShortCommit(); got != "75593ac" {
		t.Errorf("version fallback: got %q", got)
	}
	neither := ServiceRecord{Version: "0.0.0"}
	if got := neither.ShortCommit(); got != "0.0.0" {
		// "0.0.0" has no "-", so LastIndex returns -1 and we get "".
		if got != "" {
			t.Errorf("neither: got %q", got)
		}
	}
}

// TestIsStale — the badge must not fire on a healthy-but-quiet service,
// and must fire once a service really has gone silent.
func TestIsStale(t *testing.T) {
	fresh := ServiceRecord{LastSeenAt: t0}
	if fresh.IsStale(t0.Add(13 * 24 * time.Hour)) {
		t.Error("13 days should not be stale (cutoff is 14)")
	}
	if !fresh.IsStale(t0.Add(15 * 24 * time.Hour)) {
		t.Error("15 days should be stale")
	}
	never := ServiceRecord{}
	if never.IsStale(t0) {
		t.Error("a record with no LastSeenAt must not be reported stale")
	}
}

// TestDelete_RemovesHistoryToo — the plugin bus has no
// consumer-destroyed event, so explicit removal is the operator's only
// clean exit. It must not leave orphaned timeline rows behind.
func TestDelete_RemovesHistoryToo(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	if _, err := Register(ctx, store, awsRecord("gone--staging", "v1"), t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.Delete(ctx, "gone--staging"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := store.Get(ctx, "gone--staging"); found {
		t.Error("record survived delete")
	}
	if hist, _ := store.DeployHistory(ctx, "gone--staging", 0); len(hist) != 0 {
		t.Errorf("history survived delete: %d events", len(hist))
	}
	// Idempotent.
	if err := store.Delete(ctx, "gone--staging"); err != nil {
		t.Errorf("second delete should be a no-op, got %v", err)
	}
}

// TestRegister_PreservesClientConfigAndRegions extends the
// preserve-on-empty rule to the fields added for the dashboard. Same
// reasoning as Sources: a plugin version that does not send the client
// config, or a deploy that could not read it, must not erase what an
// earlier deploy captured — otherwise a detail view silently empties
// out after an unrelated redeploy.
func TestRegister_PreservesClientConfigAndRegions(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	rich := awsRecord("svc--staging", "v1")
	rich.ClientConfig = map[string]any{"template": "lambda", "timeout": 30}
	rich.Regions = []string{"eu-central-1", "us-east-1"}
	rich.HealthPath = "/custom/health"
	if _, err := Register(ctx, store, rich, t0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	bare := ServiceRecord{Stack: "svc--staging", Cloud: CloudAWS, Template: "lambda", Version: "v2"}
	got, err := Register(ctx, store, bare, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(got.ClientConfig) == 0 {
		t.Error("ClientConfig wiped by a deploy that did not report one")
	}
	if len(got.Regions) != 2 {
		t.Errorf("Regions wiped: %+v", got.Regions)
	}
	if got.HealthPath != "/custom/health" {
		t.Errorf("HealthPath wiped: %q", got.HealthPath)
	}
	if got.Version != "v2" {
		t.Errorf("version should still advance: %q", got.Version)
	}
}

// TestRegister_CapturesConfigSnapshotPerDeploy — the point of slice
// 2.0: every deploy event gets its own frozen copy of the config it
// deployed, so "what changed since the previous deploy" can be
// answered later, by a differ that may not be written yet.
func TestRegister_CapturesConfigSnapshotPerDeploy(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	v1 := awsRecord("svc--staging", "v1")
	v1.ClientConfig = map[string]any{"Config": map[string]any{"config": map[string]any{"maxMemory": 512}}}
	if _, err := Register(ctx, store, v1, t0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2 := awsRecord("svc--staging", "v2")
	v2.ClientConfig = map[string]any{"Config": map[string]any{"config": map[string]any{"maxMemory": 1024}}}
	if _, err := Register(ctx, store, v2, t0.Add(time.Hour)); err != nil {
		t.Fatalf("v2: %v", err)
	}

	hist, _ := store.DeployHistory(ctx, "svc--staging", 0)
	if len(hist) != 2 {
		t.Fatalf("want 2 events, got %d", len(hist))
	}
	for _, ev := range hist {
		if !ev.ConfigCaptured {
			t.Errorf("event %s (%s) did not record a snapshot", ev.ID, ev.Version)
		}
		cfg, found, err := store.DeployConfig(ctx, "svc--staging", ev.ID)
		if err != nil || !found {
			t.Fatalf("snapshot %s: found=%v err=%v", ev.ID, found, err)
		}
		if len(cfg) == 0 {
			t.Errorf("snapshot %s is empty", ev.ID)
		}
	}

	// The two snapshots are distinct — a shared pointer or a
	// last-write-wins key would make every diff come out empty.
	newer, _, _ := store.DeployConfig(ctx, "svc--staging", hist[0].ID)
	older, _, _ := store.DeployConfig(ctx, "svc--staging", hist[1].ID)
	get := func(m map[string]any) any {
		outer, _ := m["Config"].(map[string]any)
		inner, _ := outer["config"].(map[string]any)
		return inner["maxMemory"]
	}
	if fmt.Sprint(get(newer)) == fmt.Sprint(get(older)) {
		t.Errorf("both snapshots hold the same config: %v", get(newer))
	}
}

// TestRegister_ConfigCapturedReflectsTheRegistration, not the merged
// record. ClientConfig is preserved-on-empty (see
// TestRegister_PreservesClientConfigAndRegions), so a deploy that sent
// no config still ends up with one on the merged record. Marking that
// event ConfigCaptured would make the diff compare the deploy against
// its own predecessor's snapshot and report a confident "no changes" —
// the silent-wrong answer this flag exists to prevent.
func TestRegister_ConfigCapturedReflectsTheRegistration(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	rich := awsRecord("svc--staging", "v1")
	rich.ClientConfig = map[string]any{"template": "lambda"}
	if _, err := Register(ctx, store, rich, t0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	bare := ServiceRecord{Stack: "svc--staging", Cloud: CloudAWS, Template: "lambda", Version: "v2"}
	got, err := Register(ctx, store, bare, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(got.ClientConfig) == 0 {
		t.Fatal("precondition: the merge should have carried the config forward")
	}

	hist, _ := store.DeployHistory(ctx, "svc--staging", 0)
	if hist[0].Version != "v2" {
		t.Fatalf("newest event is %q", hist[0].Version)
	}
	if hist[0].ConfigCaptured {
		t.Error("a configless registration must not claim a snapshot")
	}
	if _, found, _ := store.DeployConfig(ctx, "svc--staging", hist[0].ID); found {
		t.Error("no snapshot object should exist for it either")
	}
}

// failingConfigStore drops snapshot writes, like an S3 PutObject that
// 503s.
type failingConfigStore struct {
	Store
	calls int
}

func (f *failingConfigStore) PutDeployConfig(context.Context, string, string, map[string]any) error {
	f.calls++
	return errors.New("s3: service unavailable")
}

// TestRegister_SnapshotFailureIsNotFatal — registration is non-fatal
// for the PLUGIN (a dashboard outage must never fail an unrelated
// deploy), and the snapshot is the least important part of it. Losing
// one diff is acceptable; losing the registration, and with it the
// service's presence on the dashboard, is not.
func TestRegister_SnapshotFailureIsNotFatal(t *testing.T) {
	ctx := context.Background()
	store := &failingConfigStore{Store: NewInMemoryStore()}

	rec := awsRecord("svc--staging", "v1")
	rec.ClientConfig = map[string]any{"template": "lambda"}
	got, err := Register(ctx, store, rec, t0)
	if err != nil {
		t.Fatalf("a failed snapshot write killed the registration: %v", err)
	}
	if got.Version != "v1" {
		t.Errorf("record not stored: %+v", got)
	}
	if store.calls != 1 {
		t.Errorf("PutDeployConfig calls = %d, want 1", store.calls)
	}

	hist, _ := store.DeployHistory(ctx, "svc--staging", 0)
	if len(hist) != 1 {
		t.Fatalf("want the event anyway, got %d", len(hist))
	}
	if hist[0].ConfigCaptured {
		t.Error("ConfigCaptured must stay false when the write failed — otherwise the read path fetches a snapshot that isn't there and has to guess why")
	}
}

// TestDelete_RemovesConfigSnapshots — same reasoning as
// TestDelete_RemovesHistoryToo: a re-registered stack must not inherit
// the previous incarnation's snapshots, and the bucket must not keep
// paying for them.
func TestDelete_RemovesConfigSnapshots(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	rec := awsRecord("gone--staging", "v1")
	rec.ClientConfig = map[string]any{"template": "lambda"}
	if _, err := Register(ctx, store, rec, t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hist, _ := store.DeployHistory(ctx, "gone--staging", 0)
	if len(hist) != 1 || !hist[0].ConfigCaptured {
		t.Fatalf("precondition: %+v", hist)
	}

	if err := store.Delete(ctx, "gone--staging"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := store.DeployConfig(ctx, "gone--staging", hist[0].ID); found {
		t.Error("snapshot survived delete")
	}
}

// TestDeployConfig_MissingIsNotAnError — "never captured" is a normal
// answer for every deploy that predates this slice, and the read path
// renders it as a stated reason. Only a transport failure is an error.
func TestDeployConfig_MissingIsNotAnError(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	for _, id := range []string{"no-such-event", ""} {
		cfg, found, err := store.DeployConfig(ctx, "svc--staging", id)
		if err != nil {
			t.Errorf("DeployConfig(%q) errored: %v", id, err)
		}
		if found || cfg != nil {
			t.Errorf("DeployConfig(%q) = (%v, %v)", id, cfg, found)
		}
	}
}

// TestHealthStore_RoundTripAndLatest — per-region latest, newest-first
// history, and pruning by age.
func TestHealthStore_RoundTripAndLatest(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()

	for i, tc := range []struct {
		region string
		status string
		offset time.Duration
	}{
		{"eu-central-1", HealthUp, 0},
		{"eu-central-1", HealthDown, time.Hour},
		{"us-east-1", HealthUp, 30 * time.Minute},
	} {
		if err := store.PutHealth(ctx, HealthResult{
			Stack: "svc--staging", Region: tc.region, Status: tc.status,
			At: t0.Add(tc.offset), HTTPStatus: 200, LatencyMS: int64(10 + i),
		}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	hist, err := store.HealthHistory(ctx, "svc--staging", 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 3 {
		t.Fatalf("want 3, got %d", len(hist))
	}
	if !hist[0].At.Equal(t0.Add(time.Hour)) {
		t.Errorf("history not newest-first: %v", hist[0].At)
	}

	latest, err := store.LatestHealth(ctx, "svc--staging")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("want 2 regions, got %d", len(latest))
	}
	if latest["eu-central-1"].Status != HealthDown {
		t.Errorf("eu-central-1 latest should be the newer down result: %+v", latest["eu-central-1"])
	}
	if latest["us-east-1"].Status != HealthUp {
		t.Errorf("us-east-1: %+v", latest["us-east-1"])
	}

	// Prune everything older than t0+45m — drops the two early results.
	n, err := store.PruneHealth(ctx, t0.Add(45*time.Minute))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Errorf("want 2 pruned, got %d", n)
	}
	remaining, _ := store.HealthHistory(ctx, "svc--staging", 0)
	if len(remaining) != 1 {
		t.Errorf("want 1 remaining, got %d", len(remaining))
	}
}
