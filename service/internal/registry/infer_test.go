// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package registry

import (
	"strings"
	"testing"
)

func logRef(t *testing.T, srcs []TelemetrySource) TelemetrySource {
	t.Helper()
	for _, s := range srcs {
		if s.Kind == KindCloudWatchLogs {
			return s
		}
	}
	t.Fatalf("no %s source in %+v", KindCloudWatchLogs, srcs)
	return TelemetrySource{}
}

// TestDeriveSources_LambdaLogGroupIsTheAccessLogsGroup is the test that
// would have caught the 2026-11-22 miss. sc-plugin-aws points the
// function's LoggingConfig at a group it names itself, so the DEFAULT
// `/aws/lambda/<fn>` group is never created — asserting the shape of the
// derived handle is the only cheap way to keep the two in step.
func TestDeriveSources_LambdaLogGroupIsTheAccessLogsGroup(t *testing.T) {
	src := logRef(t, DeriveSources("forge-conductor--staging", "lambda", "eu-central-1"))

	if src.Ref != "forge-conductor--staging-access-logs" {
		t.Errorf("Ref = %q, want the access-logs group sc-plugin-aws actually creates", src.Ref)
	}
	if strings.HasPrefix(src.Ref, "/aws/lambda/") {
		t.Error("Ref is the DEFAULT lambda log group, which sc-plugin-aws never lets a function use")
	}
	// Exact, not a prefix: the group name carries no Pulumi suffix, so a
	// DescribeLogGroups round trip per query would buy nothing.
	if src.Dims["match"] == "prefix" {
		t.Error(`handle is marked match=prefix, but the access-logs group name is exact`)
	}
	if src.Region != "eu-central-1" {
		t.Errorf("Region = %q, want it carried through", src.Region)
	}
}

// TestDeriveSources_MetricHandleStaysAPrefix — the FUNCTION name does
// carry a Pulumi suffix, unlike its log group, so slice 3 still has to
// resolve it.
func TestDeriveSources_MetricHandleStaysAPrefix(t *testing.T) {
	var found bool
	for _, s := range DeriveSources("svc--staging", "lambda", "") {
		if s.Kind != KindCloudWatchMetrics {
			continue
		}
		found = true
		if s.Dims["FunctionNamePrefix"] != "svc--staging-callback" {
			t.Errorf("FunctionNamePrefix = %q, want the suffix-free function prefix", s.Dims["FunctionNamePrefix"])
		}
		if _, ok := s.Dims["FunctionName"]; ok {
			t.Error("an exact FunctionName cannot be derived — the Pulumi suffix is not knowable here")
		}
	}
	if !found {
		t.Fatal("no metrics source derived for a lambda")
	}
}

// TestDeriveSources_ECSFargate pins all three ECS names against the
// live staging account (checked 2026-11-22 for `forge-baas--staging`:
// log group `/ecs/forge-baas--staging-staging`, cluster
// `forge-baas_staging-staging-cluster`, service
// `forge-baas_staging-staging-svc`).
//
// The doubled environment and the `--`→`_` collapse are both real; see
// ecsSources for why. The first cut of this branch derived
// `/ecs/forge-baas--staging` with `ClusterName: forge-baas--staging`
// and no ServiceName, which matches no log group and no metric — and
// would have rendered as a service with nothing to say.
func TestDeriveSources_ECSFargate(t *testing.T) {
	got := DeriveSources("forge-baas--staging", "ecs-fargate", "eu-central-1")
	if len(got) != 2 {
		t.Fatalf("want 2 sources, got %d: %+v", len(got), got)
	}

	logs := logRef(t, got)
	if logs.Ref != "/ecs/forge-baas--staging-staging" {
		t.Errorf("log group = %q", logs.Ref)
	}
	if logs.Dims["match"] == "prefix" {
		t.Error("the ECS log group is created with an explicit awslogs-group, so it is EXACT; a prefix hides a missing group as an empty match")
	}
	if logs.Region != "eu-central-1" {
		t.Errorf("region not carried: %q", logs.Region)
	}

	var metrics *TelemetrySource
	for i := range got {
		if got[i].Kind == KindCloudWatchMetrics {
			metrics = &got[i]
		}
	}
	if metrics == nil {
		t.Fatal("no metrics source derived for an ECS service")
	}
	if metrics.Ref != "AWS/ECS" {
		t.Errorf("namespace = %q", metrics.Ref)
	}
	if c := metrics.Dims["ClusterName"]; c != "forge-baas_staging-staging-cluster" {
		t.Errorf("ClusterName = %q", c)
	}
	// Both, or nothing matches: AWS/ECS publishes every metric under
	// {ClusterName, ServiceName}, and the metric provider keeps only
	// dimension sets whose key set EXACTLY equals the filter's.
	if s := metrics.Dims["ServiceName"]; s != "forge-baas_staging-staging-svc" {
		t.Errorf("ServiceName = %q", s)
	}
}

// TestDeriveSources_ECSWithoutEnvDerivesNothing — the cluster name is
// built FROM the environment, so a stack name it cannot be split out of
// yields no handles rather than approximate ones.
func TestDeriveSources_ECSWithoutEnvDerivesNothing(t *testing.T) {
	if got := DeriveSources("forge-baas", "ecs-fargate", "eu-central-1"); len(got) != 0 {
		t.Errorf("derived %+v from a stack name with no environment", got)
	}
}

// TestDeriveSources_CloudComposeIsNotATemplate — `cloud-compose` is the
// stack TYPE, and the same type deploys to GKE and k8s. Treating it as
// ECS would hand a Kubernetes workload CloudWatch handles.
func TestDeriveSources_CloudComposeIsNotATemplate(t *testing.T) {
	if got := DeriveSources("svc--staging", "cloud-compose", "eu-central-1"); len(got) != 0 {
		t.Errorf("cloud-compose derived %+v", got)
	}
}

func TestDeriveSources_UnknownTemplateDerivesNothing(t *testing.T) {
	for _, tpl := range []string{"yc-container", "kubernetes", "", "something-new"} {
		if got := DeriveSources("svc--staging", tpl, ""); len(got) != 0 {
			t.Errorf("template %q derived %+v; a guess here would be wrong more often than right", tpl, got)
		}
	}
}

func TestDeriveSources_EmptyStackDerivesNothing(t *testing.T) {
	if got := DeriveSources("  ", "lambda", ""); len(got) != 0 {
		t.Errorf("derived %+v from a blank stack name", got)
	}
}

// TestWithDerivedSources_ReplacesAnInferredGuess is the property that
// made one observatory deploy enough to fix all six tenants: the stored
// handle is treated as a cache of a guess, not as a fact.
func TestWithDerivedSources_ReplacesAnInferredGuess(t *testing.T) {
	rec := ServiceRecord{
		Stack:    "forge-storage--staging",
		Template: "lambda",
		Inferred: true,
		Sources: []TelemetrySource{{
			Kind: KindCloudWatchLogs,
			Ref:  "/aws/lambda/forge-storage--staging-callback",
			Dims: map[string]string{"match": "prefix"},
		}},
	}
	out := WithDerivedSources(rec)
	if got := logRef(t, out.Sources).Ref; got != "forge-storage--staging-access-logs" {
		t.Errorf("Ref = %q, want the stale guess replaced by this build's derivation", got)
	}
	if !out.Inferred {
		t.Error("Inferred = false; a derived handle must still render AS a guess")
	}
}

// TestWithDerivedSources_LeavesPublishedHandlesAlone. Once a template
// plugin names its own log group (decision D1), it is authoritative —
// overwriting it with a convention would be the dashboard second-
// guessing the resource's owner.
func TestWithDerivedSources_LeavesPublishedHandlesAlone(t *testing.T) {
	published := []TelemetrySource{{
		Kind: KindCloudWatchLogs, Ref: "a-group-nobody-could-guess",
	}}
	out := WithDerivedSources(ServiceRecord{
		Stack: "svc--staging", Template: "lambda",
		Inferred: false, Sources: published,
	})
	if got := logRef(t, out.Sources).Ref; got != "a-group-nobody-could-guess" {
		t.Errorf("Ref = %q, want the published handle untouched", got)
	}
	if out.Inferred {
		t.Error("Inferred flipped to true on a published handle")
	}
}

// TestWithDerivedSources_FillsAnEmptyRecord — covers a registration
// written before anything derived handles at all.
func TestWithDerivedSources_FillsAnEmptyRecord(t *testing.T) {
	out := WithDerivedSources(ServiceRecord{Stack: "svc--staging", Template: "lambda"})
	if len(out.Sources) == 0 {
		t.Fatal("no handles derived for a record that had none")
	}
	if !out.Inferred {
		t.Error("Inferred = false on handles this build invented")
	}
}

// TestWithDerivedSources_KeepsStoredHandlesWhenNothingIsDerivable. An
// underivable template means this build knows LESS than the registration
// did, not more — blanking the record would lose a working handle.
func TestWithDerivedSources_KeepsStoredHandlesWhenNothingIsDerivable(t *testing.T) {
	stored := []TelemetrySource{{Kind: "yc-logging", Ref: "e23abc"}}
	out := WithDerivedSources(ServiceRecord{
		Stack: "svc--staging", Template: "yc-container",
		Inferred: true, Sources: stored,
	})
	if len(out.Sources) != 1 || out.Sources[0].Ref != "e23abc" {
		t.Errorf("Sources = %+v, want the stored handle kept", out.Sources)
	}
}

// TestWithDerivedSources_UsesTheRecordedRegion — a handle derived
// without a region falls back to the provider's default, which is wrong
// the moment a service is deployed outside it.
func TestWithDerivedSources_UsesTheRecordedRegion(t *testing.T) {
	out := WithDerivedSources(ServiceRecord{
		Stack: "svc--staging", Template: "lambda",
		Regions: []string{"us-east-1", "eu-central-1"},
	})
	if got := logRef(t, out.Sources).Region; got != "us-east-1" {
		t.Errorf("Region = %q, want the record's first region", got)
	}
}
