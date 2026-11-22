// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import "testing"

func TestCloudForTemplate(t *testing.T) {
	for _, tc := range []struct {
		template string
		cloud    string
		ok       bool
	}{
		{"lambda", CloudAWS, true},
		{"aws-lambda", CloudAWS, true},
		{"ecs-fargate", CloudAWS, true},
		{"yc-container", CloudYandex, true},
		{"kubernetes", CloudKubernetes, true},
		{"gke-autopilot", CloudKubernetes, true},
		{"  LAMBDA  ", CloudAWS, true}, // tolerant of case + padding
		{"vm-compose", "", false},      // genuinely unknown
		{"", "", false},
	} {
		cloud, ok := CloudForTemplate(tc.template)
		if cloud != tc.cloud || ok != tc.ok {
			t.Errorf("CloudForTemplate(%q) = (%q,%v), want (%q,%v)", tc.template, cloud, ok, tc.cloud, tc.ok)
		}
	}
}

// TestInferSources_LambdaLogGroupIsExact — the log group is the one
// sc-plugin-aws names itself (`<stack>-access-logs`), not the default
// `/aws/lambda/<function-name>`, which it never lets a function use.
// The name carries no Pulumi suffix, so the handle is EXACT.
//
// This asserts the corrected convention. The first cut emitted the
// `/aws/lambda/<stack>-callback` prefix and every tenant's log query
// came back "no log group could be resolved" — the staging account
// holds zero `/aws/lambda/*` groups. Live-caught 2026-11-22.
func TestInferSources_LambdaLogGroupIsExact(t *testing.T) {
	got := InferSources("forge-storage--staging", "lambda", "eu-central-1")
	if len(got) != 2 {
		t.Fatalf("want logs+metrics sources, got %d: %+v", len(got), got)
	}
	logs := got[0]
	if logs.Kind != KindCloudWatchLogs {
		t.Errorf("first source should be logs, got %q", logs.Kind)
	}
	if logs.Ref != "forge-storage--staging-access-logs" {
		t.Errorf("log group ref: got %q", logs.Ref)
	}
	if logs.Dims["match"] == "prefix" {
		t.Error("the access-logs group name is exact; a prefix match buys a DescribeLogGroups round trip for nothing")
	}
	if logs.Region != "eu-central-1" {
		t.Errorf("region not carried: %q", logs.Region)
	}
	metrics := got[1]
	if metrics.Kind != KindCloudWatchMetrics || metrics.Ref != "AWS/Lambda" {
		t.Errorf("metrics source wrong: %+v", metrics)
	}
	// FunctionName must NOT be asserted as exact — only as a prefix the
	// provider resolves. Claiming an exact dimension we cannot know
	// would silently return empty metric series.
	if _, exact := metrics.Dims["FunctionName"]; exact {
		t.Error("metrics must not claim an exact FunctionName; the suffix is unknown")
	}
	if metrics.Dims["FunctionNamePrefix"] == "" {
		t.Error("metrics should carry a FunctionNamePrefix for the provider to resolve")
	}
}

// TestInferSources_Fargate pins the three ECS names against the live
// staging account (checked 2026-11-22 for `forge-baas--staging`).
//
// The expected strings look mangled and are not: the environment
// appears TWICE because sc-plugin-aws appends it to a stack name that
// already ends in `--<env>`, and the `--` survives in the log group but
// collapses to `_` in the cluster and service names because those go
// through its `awsResName`. The first cut of this branch guessed all
// three (`/ecs/svc--staging`, `ClusterName: svc--staging`, no
// ServiceName at all) and would have matched nothing.
func TestInferSources_Fargate(t *testing.T) {
	got := InferSources("forge-baas--staging", "ecs-fargate", "eu-central-1")
	if len(got) != 2 {
		t.Fatalf("want 2 sources, got %d", len(got))
	}
	if got[0].Kind != KindCloudWatchLogs || got[0].Ref != "/ecs/forge-baas--staging-staging" {
		t.Errorf("ecs log group: %+v", got[0])
	}
	// Exact, not a prefix: the template creates the group with an
	// explicit `awslogs-group`, so there is no Pulumi suffix to resolve
	// and a missing group should surface by name.
	if got[0].Dims["match"] == "prefix" {
		t.Error("the ECS log group name is exact; marking it a prefix hides a missing group behind an empty match")
	}
	if got[1].Kind != KindCloudWatchMetrics || got[1].Ref != "AWS/ECS" {
		t.Fatalf("ecs metrics source wrong: %+v", got[1])
	}
	if c := got[1].Dims["ClusterName"]; c != "forge-baas_staging-staging-cluster" {
		t.Errorf("ClusterName: %q", c)
	}
	// Both dimensions, or none match: the provider keeps only dimension
	// sets whose key set EXACTLY equals the filter's, and every AWS/ECS
	// metric is published under {ClusterName, ServiceName}.
	if s := got[1].Dims["ServiceName"]; s != "forge-baas_staging-staging-svc" {
		t.Errorf("ServiceName: %q", s)
	}
}

// TestInferSources_FargateWithoutEnv — a stack name with no `--<env>`
// cannot produce a cluster name, and half a name matches nothing. No
// handles is the honest answer.
func TestInferSources_FargateWithoutEnv(t *testing.T) {
	if got := InferSources("forge-baas", "ecs-fargate", "eu-central-1"); got != nil {
		t.Errorf("want no sources without an environment, got %+v", got)
	}
}

// TestInferSources_CloudComposeIsNotAnECSTemplate — `cloud-compose` is
// a TYPE and it deploys to GKE and k8s too. Accepting it as a template
// here would hand a Kubernetes workload CloudWatch handles, which is
// the wrong-resource failure this component exists to prevent.
func TestInferSources_CloudComposeIsNotAnECSTemplate(t *testing.T) {
	if got := InferSources("svc--staging", "cloud-compose", "eu-central-1"); got != nil {
		t.Errorf("cloud-compose must not infer AWS handles, got %+v", got)
	}
}

// TestInferSources_ReturnsNothingRatherThanGuessing — for YC and k8s
// there is no derivable handle (a YC log group is an opaque id; k8s
// namespace conventions vary per operator). Returning nothing is the
// honest answer: the service renders "no telemetry wired" and the
// operator knows to look, instead of seeing an empty log view that
// reads as a quiet service.
func TestInferSources_ReturnsNothingRatherThanGuessing(t *testing.T) {
	for _, template := range []string{"yc-container", "kubernetes", "gke-autopilot", "vm-compose", "nonsense"} {
		if got := InferSources("svc--staging", template, "ru-central1-a"); got != nil {
			t.Errorf("InferSources(%q) should infer nothing, got %+v", template, got)
		}
	}
}
