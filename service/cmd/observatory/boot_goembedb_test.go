// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/FerretDB/FerretDB/ferretdb"
)

// TestBootGoEmbedOnce_LambdaWithoutBucketHardFails pins the invariant that
// a Lambda deploy with GOEMBEDB_S3_BUCKET unset refuses to boot rather
// than warning and falling back to /tmp/goembedb. Lambda's /tmp is per-
// container and dies with every container recycle; silent fallback means
// each warm container runs its own ephemeral store. For observatory that
// means up/down edge state resets across cold starts and produces
// spurious alerts — the exact shape this component's alerting exists to
// detect.
//
// Inversion to confirm the test can fail: unset AWS_LAMBDA_FUNCTION_NAME
// (the sibling test TestBootGoEmbedOnce_NonLambdaWithoutBucketWarns);
// bootGoEmbedOnce must NOT return the GOEMBEDB_S3_BUCKET-named error in
// that mode. Without the inversion, this test would also pass against
// the pre-fix code that unconditionally warned.
func TestBootGoEmbedOnce_LambdaWithoutBucketHardFails(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOEMBEDB_SQLITE_DIR", tmp)
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "observatory-staging-test")
	t.Setenv("GOEMBEDB_S3_BUCKET", "")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, err := bootGoEmbedOnce(logger)
	if err == nil {
		t.Fatal("bootGoEmbedOnce must refuse to boot on Lambda with no bucket; got nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "GOEMBEDB_S3_BUCKET") {
		t.Errorf("error must name GOEMBEDB_S3_BUCKET so the operator can find the fix; got: %s", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "lambda") {
		t.Errorf("error must name Lambda so the Lambda-specific reason is visible; got: %s", msg)
	}
}

// TestBootGoEmbedOnce_NonLambdaWithoutBucketWarns inverts the assertion:
// outside Lambda (ECS / VM / local smoke) a single-node sqlite embed is
// a valid "just put it in sqlite" deploy, so no-bucket must proceed with
// a warning rather than return the Lambda error.
//
// Without this inversion, a pre-fix implementation that unconditionally
// returned the GOEMBEDB_S3_BUCKET error would pass the Lambda-hard-fail
// test and silently break every non-Lambda deploy.
func TestBootGoEmbedOnce_NonLambdaWithoutBucketWarns(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOEMBEDB_SQLITE_DIR", tmp)
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "")
	t.Setenv("GOEMBEDB_S3_BUCKET", "")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_, err := bootGoEmbedOnce(logger)
	if err != nil && strings.Contains(err.Error(), "GOEMBEDB_S3_BUCKET") &&
		strings.Contains(strings.ToLower(err.Error()), "lambda") {
		t.Errorf("non-Lambda no-bucket must NOT return the Lambda error; got: %s", err)
	}
}

// TestApplyReplicationKnobs_LambdaDefaultsRecoveryTimeout pins the knob the
// go-embedb #32 pin makes load-bearing. The engine's own default is 90s,
// chosen to stay under a typical function timeout; observatory's timeout is
// 300s, and the FIRST cold start after the #32 pin replays every slot from
// zero because pre-#32 snapshots carry no SnapshotMeta.AppliedOffsets
// watermark. 90s would very plausibly not finish that one start, and an
// overrunning replay is deliberately fatal inside the engine — so a boot
// that leaves RecoveryTimeout at the engine default is a boot that can lose
// the whole migrated store again.
//
// It also pins "strictly below the Lambda timeout": the engine must be the
// thing that gives up first, so the failure is a readable boot error rather
// than a request timeout with nothing logged.
//
// Inversion to confirm the test can fail: drop the `rep.RecoveryTimeout =
// 240 * time.Second` assignment from applyReplicationKnobs and this test
// reports `RecoveryTimeout = 0s`, naming the engine default it falls back
// to. Dropping the whole `if lambda` block fails here AND in
// TestApplyReplicationKnobs_NonLambdaLeavesDefaults' sibling assertions.
func TestApplyReplicationKnobs_LambdaDefaultsRecoveryTimeout(t *testing.T) {
	t.Setenv("GOEMBEDB_RECOVERY_TIMEOUT", "")
	t.Setenv("GOEMBEDB_HEARTBEAT_TTL", "")
	t.Setenv("GOEMBEDB_MAX_REPLICAS", "")
	t.Setenv("GOEMBEDB_ACK_WAIT_LIVE_ONLY", "")

	rep := &ferretdb.ReplicationStorageConfig{Backend: "s3", S3Bucket: "b"}
	applyReplicationKnobs(rep, true)

	if rep.RecoveryTimeout != 240*time.Second {
		t.Errorf("Lambda default RecoveryTimeout = %s, want 240s (engine default 90s is too tight for the first post-#32 replay-from-zero)", rep.RecoveryTimeout)
	}
	// The Lambda function timeout is 300s (client.yaml, raised for the
	// chunked migration). Strictly below, not equal.
	if rep.RecoveryTimeout >= 300*time.Second {
		t.Errorf("RecoveryTimeout = %s must stay strictly below the 300s Lambda timeout so the engine fails loudly first", rep.RecoveryTimeout)
	}
	if !rep.AckWaitLiveOnly {
		t.Error("Lambda must set AckWaitLiveOnly: a frozen predecessor container runs no heartbeat goroutine, so waiting on its ack never returns")
	}
	// 15m, derived from the `hc-tick` EventBridge rule's rate(5 minutes).
	//
	// Raised from 3m on 2026-11-25. The heartbeat is no longer renewed by
	// a ticker goroutine — Lambda freezes those — but synchronously from
	// the tick handler, so the TTL must outlive the DRIVING cadence, not
	// the old 30s interval. At 3m against a 5-minute tick the slot was
	// "renewed and then expired for 2 of every 5 minutes", which looks
	// identical from the outside to a slot that renews fine.
	//
	// Keep this at >= 3x the rule's rate: Handler.TickNow measures the
	// interval between its own calls and warns when TTL < 2x the observed
	// cadence, so a drift between the two is loud rather than silent.
	if rep.HeartbeatTTL != 15*time.Minute {
		t.Errorf("Lambda default HeartbeatTTL = %s, want 15m (>= 3x the hc-tick rate(5 minutes) that now drives the renewal)", rep.HeartbeatTTL)
	}
	if rep.MaxReplicas != 10 {
		t.Errorf("Lambda default MaxReplicas = %d, want 10", rep.MaxReplicas)
	}
}

// TestApplyReplicationKnobs_EnvOverridesWin inverts the default: an
// explicitly-set GOEMBEDB_RECOVERY_TIMEOUT must survive the Lambda branch.
// Without this, a future edit could reasonably hard-set the Lambda value
// unconditionally and leave the operator with no way to shorten a replay
// mid-incident.
//
// Inversion to confirm the test can fail: move the
// envDurationOrZero("GOEMBEDB_RECOVERY_TIMEOUT") read to AFTER the
// `if lambda` block (or drop the `== 0` guard on the default) and this
// reports 240s where 45s was asked for.
func TestApplyReplicationKnobs_EnvOverridesWin(t *testing.T) {
	t.Setenv("GOEMBEDB_RECOVERY_TIMEOUT", "45s")
	t.Setenv("GOEMBEDB_HEARTBEAT_TTL", "30s")
	t.Setenv("GOEMBEDB_MAX_REPLICAS", "4")

	rep := &ferretdb.ReplicationStorageConfig{Backend: "s3", S3Bucket: "b"}
	applyReplicationKnobs(rep, true)

	if rep.RecoveryTimeout != 45*time.Second {
		t.Errorf("explicit GOEMBEDB_RECOVERY_TIMEOUT=45s lost: got %s", rep.RecoveryTimeout)
	}
	if rep.HeartbeatTTL != 30*time.Second {
		t.Errorf("explicit GOEMBEDB_HEARTBEAT_TTL=30s lost: got %s", rep.HeartbeatTTL)
	}
	if rep.MaxReplicas != 4 {
		t.Errorf("explicit GOEMBEDB_MAX_REPLICAS=4 lost: got %d", rep.MaxReplicas)
	}
}

// TestApplyReplicationKnobs_NonLambdaLeavesDefaults is the inversion that
// keeps the Lambda-correctness defaults from leaking into a long-lived
// deploy (ECS / VM / local smoke). There, a container is not frozen between
// requests: the heartbeat goroutine runs, so AckWaitLiveOnly is not needed,
// and the engine's own RecoveryTimeout is the right one to inherit.
//
// Zero means "engine default", not "no timeout" — go-embedb substitutes
// DefaultReplayTimeout (90s) via recoveryTimeoutOrDefault.
//
// Inversion to confirm the test can fail: make the knob block
// unconditional (drop `if lambda`) and this reports 240s / live-only true
// outside Lambda, while the Lambda test above still passes — one redeploy
// green on exactly the failure the pair flags.
func TestApplyReplicationKnobs_NonLambdaLeavesDefaults(t *testing.T) {
	t.Setenv("GOEMBEDB_RECOVERY_TIMEOUT", "")
	t.Setenv("GOEMBEDB_HEARTBEAT_TTL", "")
	t.Setenv("GOEMBEDB_MAX_REPLICAS", "")
	t.Setenv("GOEMBEDB_ACK_WAIT_LIVE_ONLY", "")

	rep := &ferretdb.ReplicationStorageConfig{Backend: "s3", S3Bucket: "b"}
	applyReplicationKnobs(rep, false)

	if rep.RecoveryTimeout != 0 {
		t.Errorf("non-Lambda RecoveryTimeout = %s, want 0 so go-embedb's own DefaultReplayTimeout applies", rep.RecoveryTimeout)
	}
	if rep.AckWaitLiveOnly {
		t.Error("non-Lambda must not force AckWaitLiveOnly; a long-lived host runs its heartbeat goroutine")
	}
	if rep.HeartbeatTTL != 0 {
		t.Errorf("non-Lambda HeartbeatTTL = %s, want 0 (engine default)", rep.HeartbeatTTL)
	}
	if rep.MaxReplicas != 0 {
		t.Errorf("non-Lambda MaxReplicas = %d, want 0 (engine default)", rep.MaxReplicas)
	}
}

// TestApplyReplicationKnobs_AckWaitLiveOnlyEnvWorksOutsideLambda keeps the
// documented escape hatch honest: a non-Lambda deploy that nonetheless
// wants live-only acks (a smoke, say) can ask for it.
//
// Inversion to confirm the test can fail: move the envBool read inside the
// `if lambda` block and this reports false.
func TestApplyReplicationKnobs_AckWaitLiveOnlyEnvWorksOutsideLambda(t *testing.T) {
	t.Setenv("GOEMBEDB_ACK_WAIT_LIVE_ONLY", "1")

	rep := &ferretdb.ReplicationStorageConfig{Backend: "s3", S3Bucket: "b"}
	applyReplicationKnobs(rep, false)

	if !rep.AckWaitLiveOnly {
		t.Error("GOEMBEDB_ACK_WAIT_LIVE_ONLY=1 must apply outside Lambda too")
	}
}
