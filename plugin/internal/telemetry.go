// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"fmt"
	"strings"
)

// TelemetrySource mirrors the service's registry.TelemetrySource. The
// plugin and service are separate modules, so the shape is declared on
// both sides and carried over HTTP as JSON.
type TelemetrySource struct {
	Kind   string            `json:"kind"`
	Ref    string            `json:"ref"`
	Region string            `json:"region,omitempty"`
	Dims   map[string]string `json:"dims,omitempty"`
}

// Telemetry source kinds. One per queryable backend, matching the
// provider implementations on the service side.
const (
	KindCloudWatchLogs    = "cloudwatch-logs"
	KindCloudWatchMetrics = "cloudwatch-metrics"
	KindYCLogging         = "yc-logging"
	KindYCMonitoring      = "yc-monitoring"
	KindK8sPodLogs        = "k8s-pod-logs"
)

// Cloud tokens.
const (
	CloudAWS        = "aws"
	CloudYandex     = "yandex"
	CloudKubernetes = "kubernetes"
)

// PublishedSourcesKey is the compute-context key a template plugin is
// expected to publish its telemetry handles under, as a JSON array.
//
// Sibling to the `_endpoint_url` (ComponentEndpointURLKey) that
// sc-plugin-aws already publishes — the precedent this follows. Nothing
// writes it yet; wiring each template plugin to do so is design
// decision D1 / slice 4. Until then InferSources below guesses, and the
// guess is flagged.
const PublishedSourcesKey = "_telemetry_sources"

// CloudForTemplate maps an SC template name to the cloud whose
// telemetry APIs serve it.
//
// Template, not cloud, is the input: the permission and telemetry
// shapes differ per template (a Lambda's log group is not an ECS task's)
// and two templates on the same cloud need different handling. An
// unknown template returns ok=false so the caller records "no
// telemetry" rather than guessing wrong and rendering an empty chart
// that reads as "no traffic".
func CloudForTemplate(template string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(template)) {
	case "lambda", "aws-lambda", "ecs-fargate", "aws-static-website", "single-image":
		return CloudAWS, true
	case "yc-container", "yc-serverless-container", "yc-static-website":
		return CloudYandex, true
	case "kubernetes", "gke-autopilot", "k8s-run":
		return CloudKubernetes, true
	default:
		return "", false
	}
}

// InferSources derives telemetry handles from deterministic naming,
// for use until template plugins publish them (decision D1, slice 4).
//
// This is a GUESS and every caller must mark the resulting record
// `inferred: true` so the dashboard can say so. The alternative —
// presenting a guessed log group as authoritative — produces the worst
// possible failure: an empty log view that looks like a quiet service
// rather than a wrong lookup. That exact class of silent degradation
// is what cost the first SC Component three deploy attempts.
//
// Returns nil when nothing can be inferred, which is honest: the
// service renders "no telemetry wired" and an operator knows to look.
//
// AWS: SC names a Lambda `<resScope>-callback-<suffix>` where the
// suffix is Pulumi-generated and NOT derivable here — so the DEFAULT
// log group `/aws/lambda/<function-name>` would only be reachable by
// prefix. But sc-plugin-aws never lets the default group be used: it
// creates `<resScope>-access-logs` with an explicit Name (no Pulumi
// suffix) and points the function's LoggingConfig.LogGroup at it
// (sc-plugin-aws/plugin/internal/aws_lambda.go:436-461). `resScope` is
// the same value the function name is built from, so the group is
// derivable EXACTLY.
//
// This was live-caught 2026-11-22: the first cut emitted the
// `/aws/lambda/<stack>-callback` prefix, the staging account holds zero
// `/aws/lambda/*` groups, and every tenant's log query came back "no
// log group could be resolved".
//
// The service re-derives these handles at read time
// (registry.WithDerivedSources), so the two copies of the convention
// must agree — but the service's copy is the one that decides, and it
// can be corrected without redeploying a single tenant.
func InferSources(stack, template, region string) []TelemetrySource {
	cloud, ok := CloudForTemplate(template)
	if !ok {
		return nil
	}
	switch cloud {
	case CloudAWS:
		switch strings.ToLower(template) {
		case "ecs-fargate":
			// ECS task definitions created by sc-plugin-aws log to
			// `/ecs/<family>`; the family is the stack name.
			return []TelemetrySource{{
				Kind:   KindCloudWatchLogs,
				Ref:    "/ecs/" + stack,
				Region: region,
				Dims:   map[string]string{"ClusterName": stack, "match": "prefix"},
			}, {
				Kind:   KindCloudWatchMetrics,
				Ref:    "AWS/ECS",
				Region: region,
				Dims:   map[string]string{"ClusterName": stack},
			}}
		default:
			return []TelemetrySource{{
				Kind:   KindCloudWatchLogs,
				Ref:    fmt.Sprintf("%s-access-logs", stack),
				Region: region,
			}, {
				Kind:   KindCloudWatchMetrics,
				Ref:    "AWS/Lambda",
				Region: region,
				// FunctionName is intentionally absent: the Pulumi
				// suffix is unknown, so the provider must resolve the
				// concrete function from the log-group prefix above
				// before it can dimension a metric query.
				Dims: map[string]string{"match": "prefix", "FunctionNamePrefix": stack + "-callback"},
			}}
		}

	case CloudYandex:
		// YC log groups are referenced by id, which is not derivable
		// from a stack name at all. Returning nothing is the honest
		// answer; slice 6 gets real handles from sc-plugin-yandex.
		return nil

	case CloudKubernetes:
		// Namespace/selector conventions vary per operator, so a guess
		// here would be wrong more often than right.
		return nil
	}
	return nil
}
