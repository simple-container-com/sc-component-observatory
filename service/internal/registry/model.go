// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package registry holds observatory's own system of record: which
// services are deployed, where, at what version, and what telemetry
// handles they expose.
//
// This is NOT telemetry storage. Logs and metrics are read from the
// target cloud's own stores at query time and never ingested here.
package registry

import "time"

// Cloud identifies which telemetry provider can serve a service.
// Recorded per-service because one fleet spans several.
const (
	CloudAWS        = "aws"
	CloudYandex     = "yandex"
	CloudKubernetes = "kubernetes"
)

// TelemetrySource is one queryable telemetry handle for a deployed
// workload. Opaque to observatory except for Kind, which selects the
// provider path, and Dims, which become metric dimensions.
//
// The whole design rests on observatory never DERIVING these: the
// plugin that created a resource is the only thing that reliably knows
// its identifiers. Until every template plugin publishes them
// (design doc §6.A / decision D1), they are inferred from deterministic
// naming and the record is marked Inferred — a guess that renders AS a
// guess rather than looking authoritative.
type TelemetrySource struct {
	// Kind selects the provider path: "cloudwatch-logs",
	// "cloudwatch-metrics", "yc-logging", "yc-monitoring",
	// "k8s-pod-logs", "k8s-metrics", "prometheus".
	Kind string `bson:"kind" json:"kind"`

	// Ref is the handle itself — a CloudWatch log-group name, a YC
	// log-group id, a `namespace/selector` pair. Provider-specific.
	Ref string `bson:"ref" json:"ref"`

	// Dims carries metric dimensions / label selectors, e.g.
	// {"FunctionName": "..."} for CloudWatch or
	// {"container_id": "..."} for YC Monitoring.
	Dims map[string]string `bson:"dims,omitempty" json:"dims,omitempty"`

	// Region matters for AWS, where log groups and metrics are
	// per-region. Empty means "the provider's default region".
	Region string `bson:"region,omitempty" json:"region,omitempty"`
}

// ServiceRecord is one deployed service as observatory knows it.
//
// Written by the plugin's ComputeProcess on every consumer deploy
// (push-register, mirroring sc-component-backup's tenant.Record) and
// read by the dashboard.
type ServiceRecord struct {
	// Stack is the SC stack name and the primary key, e.g.
	// "forge-notifier--staging". Unique.
	Stack string `bson:"stack" json:"stack"`

	// Service and Env are Stack split on the "--" separator SC uses,
	// pre-computed so the dashboard can group without re-parsing.
	Service string `bson:"service" json:"service"`
	Env     string `bson:"env" json:"env"`

	// Cloud selects the telemetry provider. One of the Cloud* consts.
	Cloud string `bson:"cloud" json:"cloud"`

	// Template is the SC template that deployed it — "lambda",
	// "ecs-fargate", "yc-container", "kubernetes", "vm-compose".
	// Kept alongside Cloud because the permission and telemetry shapes
	// differ per template, not just per cloud.
	Template string `bson:"template" json:"template"`

	// Version is the deployed version. In CI this is SC's CalVer,
	// `YYYY.MM.DD-<short-sha>` — SC injects VERSION as a build arg on
	// EVERY image build, so it is always present on a CI deploy. A
	// value of "0.0.0" means the Dockerfile default leaked through and
	// is a bug, not a normal reading.
	Version string `bson:"version" json:"version"`

	// Commit is the full git SHA. NOT auto-injected by SC — the deploy
	// action exports GITHUB_SHA and services declare it under
	// `image.build.args`. When absent, CommitFromVersion derives the
	// short SHA out of Version instead.
	Commit string `bson:"commit,omitempty" json:"commit,omitempty"`

	// DeployedAt is when this version registered. OBSERVED by
	// observatory at ComputeProcess time, never self-reported, so a
	// service cannot misrepresent its own freshness.
	DeployedAt time.Time `bson:"deployedAt" json:"deployed_at"`

	// FirstSeenAt is the first registration ever for this stack.
	// Preserved across re-registrations.
	FirstSeenAt time.Time `bson:"firstSeenAt" json:"first_seen_at"`

	// LastSeenAt is the most recent registration of any kind. Drives
	// the staleness badge: the plugin bus has no "consumer destroyed"
	// event, so a service that stops re-registering is the only signal
	// that it may be gone.
	LastSeenAt time.Time `bson:"lastSeenAt" json:"last_seen_at"`

	// EndpointURL is the service's public URL when the template
	// published one (AWS Lambda Function URL, YC container URL).
	EndpointURL string `bson:"endpointUrl,omitempty" json:"endpoint_url,omitempty"`

	// Sources are the telemetry handles. May be empty for a service
	// whose template publishes none and whose naming we cannot infer.
	Sources []TelemetrySource `bson:"sources,omitempty" json:"sources,omitempty"`

	// Inferred marks Sources as derived from naming conventions rather
	// than published by the owning template plugin. The dashboard shows
	// a warning badge so a wrong guess is visible instead of silently
	// returning no logs.
	Inferred bool `bson:"inferred,omitempty" json:"inferred,omitempty"`
}

// DeployEvent is one append-only registration. The dashboard renders
// these as a per-service timeline — what version landed when, from
// which commit — which is the single most-asked-for thing on a deploy
// dashboard and costs one small object per deploy.
type DeployEvent struct {
	// ID is a stable identifier derived from stack + timestamp so a
	// retried registration does not double-count.
	ID         string    `bson:"_id" json:"id"`
	Stack      string    `bson:"stack" json:"stack"`
	Version    string    `bson:"version" json:"version"`
	Commit     string    `bson:"commit,omitempty" json:"commit,omitempty"`
	Template   string    `bson:"template" json:"template"`
	Cloud      string    `bson:"cloud" json:"cloud"`
	DeployedAt time.Time `bson:"deployedAt" json:"deployed_at"`

	// PreviousVersion is what this deploy replaced, empty on the first
	// registration. Lets the timeline show version transitions without
	// the reader diffing adjacent rows.
	PreviousVersion string `bson:"previousVersion,omitempty" json:"previous_version,omitempty"`
}
