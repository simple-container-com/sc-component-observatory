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

// TestInferSources_LambdaUsesPrefixMatch — the Pulumi-generated
// function suffix is not derivable, so the inferred log-group handle
// must be a PREFIX and must say so, or the AWS provider will treat it
// as an exact group name and find nothing.
func TestInferSources_LambdaUsesPrefixMatch(t *testing.T) {
	got := InferSources("forge-storage--staging", "lambda", "eu-central-1")
	if len(got) != 2 {
		t.Fatalf("want logs+metrics sources, got %d: %+v", len(got), got)
	}
	logs := got[0]
	if logs.Kind != KindCloudWatchLogs {
		t.Errorf("first source should be logs, got %q", logs.Kind)
	}
	if logs.Ref != "/aws/lambda/forge-storage--staging-callback" {
		t.Errorf("log group ref: got %q", logs.Ref)
	}
	if logs.Dims["match"] != "prefix" {
		t.Error("an inferred Lambda log group MUST be flagged as a prefix — the Pulumi suffix is unknown")
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

// TestInferSources_Fargate — ECS logs to /ecs/<family>.
func TestInferSources_Fargate(t *testing.T) {
	got := InferSources("svc--staging", "ecs-fargate", "eu-west-1")
	if len(got) != 2 {
		t.Fatalf("want 2 sources, got %d", len(got))
	}
	if got[0].Ref != "/ecs/svc--staging" {
		t.Errorf("ecs log group: %q", got[0].Ref)
	}
	if got[1].Ref != "AWS/ECS" || got[1].Dims["ClusterName"] != "svc--staging" {
		t.Errorf("ecs metrics: %+v", got[1])
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
