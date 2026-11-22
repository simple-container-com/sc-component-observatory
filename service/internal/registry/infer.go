// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package registry

import "strings"

// Telemetry source kinds. Mirrors the plugin's constants of the same
// name; the two modules carry the shape over HTTP rather than sharing a
// package.
const (
	KindCloudWatchLogs    = "cloudwatch-logs"
	KindCloudWatchMetrics = "cloudwatch-metrics"
)

// DeriveSources returns the telemetry handles that follow from SC's own
// resource-naming conventions for a deployed stack.
//
// # Why this lives on the SERVICE side and not only in the plugin
//
// The plugin derives the same handles at registration time, and until
// decision D1 lands (template plugins publishing `_telemetry_sources`)
// a derivation is all anyone has. But a registration-time guess is
// frozen into the stored record, and correcting it then requires
// redeploying every tenant — one deploy per service, each gated on
// whatever else is in flight in that repo.
//
// That is not hypothetical. On 2026-11-22 the Lambda log handle shipped
// wrong (see below), and all six registered tenants carried the wrong
// group. Re-deriving at READ time made one observatory deploy enough.
// So: the convention is applied where it can be corrected, and the
// stored handles are treated as a cache of a guess, not as a fact.
//
// Published handles are never touched — see WithDerivedSources.
func DeriveSources(stack, template, region string) []TelemetrySource {
	stack = strings.TrimSpace(stack)
	if stack == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(template)) {
	case "lambda", "aws-lambda", "single-image", "aws-static-website":
		// The log group is EXACT, not a prefix.
		//
		// This is the opposite of what the first cut assumed. SC names a
		// Lambda `<resScope>-callback-<pulumi-suffix>`, so the DEFAULT
		// group `/aws/lambda/<function-name>` would indeed only be
		// reachable by prefix — except that sc-plugin-aws never lets the
		// default group be used. It creates `<resScope>-access-logs` with
		// an explicit Name (so Pulumi adds no suffix) and points the
		// function's LoggingConfig.LogGroup at it:
		// sc-plugin-aws/plugin/internal/aws_lambda.go:436-461.
		//
		// Consequence, live-caught 2026-11-22 on monitor.simple-forge.com:
		// the account contains ZERO `/aws/lambda/*` log groups, so the
		// prefix resolved to nothing for every tenant and every log query
		// came back "no log group could be resolved". `resScope` is the
		// same value the function name is built from, so `-access-logs`
		// is derivable exactly — no resolution call, and a missing group
		// surfaces by name instead of as an empty prefix match.
		return []TelemetrySource{{
			Kind:   KindCloudWatchLogs,
			Ref:    stack + "-access-logs",
			Region: region,
		}, {
			Kind:   KindCloudWatchMetrics,
			Ref:    "AWS/Lambda",
			Region: region,
			// FunctionName is intentionally absent: the Pulumi suffix is
			// not derivable, so slice 3 resolves the concrete function
			// from this prefix before it can dimension a metric query.
			Dims: map[string]string{"match": "prefix", "FunctionNamePrefix": stack + "-callback"},
		}}

	// `ecs-fargate` ONLY, deliberately: the Lambda branch above also
	// accepts a couple of `type:` spellings as a leftover safety net,
	// but `cloud-compose` must NOT be added here. It is a TYPE, and the
	// same type deploys to GKE or k8s with a different template — so
	// accepting it would hand a Kubernetes workload CloudWatch handles.
	case "ecs-fargate":
		return ecsSources(stack, region)
	}

	// Yandex log groups are referenced by id, and k8s namespace/selector
	// conventions vary per operator — neither is derivable from a stack
	// name. Nothing is the honest answer: the reader gets "no telemetry
	// handles are recorded", not an empty log view.
	return nil
}

// ecsSources derives the handles sc-plugin-aws's `ecs-fargate` template
// creates for a `cloud-compose` stack.
//
// Every name below was checked against the live staging account on
// 2026-11-22 before it shipped, because the first cut of this branch
// (written from the stack name alone) got all three wrong — and an ECS
// tenant is exactly the case where a wrong guess is least likely to be
// noticed, since the panels degrade to "nothing matched" rather than to
// an error.
//
// The rule is one line of sc-plugin-aws and two surprises:
//
//	ecsSimpleClusterName := fmt.Sprintf("%s-%s", stack.Name, env)   // ecs_fargate.go:208
//
// 1. `stack.Name` ALREADY ENDS IN `--<env>` — it is the same value this
// package receives as `rec.Stack` (`forge-baas--staging`), and the same
// one the Lambda branch above appends `-callback` to. So the simple
// cluster name carries the environment TWICE:
// `forge-baas--staging-staging`. That doubling is upstream's and is not
// a typo here.
//
// 2. The cluster and service names then go through sc-plugin-aws's
// `awsResName`, which COLLAPSES the `--` to an underscore — so the ECS
// names are spelled differently from the log group, which keeps the
// `--`. Live, for `forge-baas--staging`:
//
//	log group  /ecs/forge-baas--staging-staging        (exact: created
//	                                                    with an explicit
//	                                                    awslogs-group)
//	cluster    forge-baas_staging-staging-cluster
//	service    forge-baas_staging-staging-svc
//
// The log group is EXACT for the same reason the Lambda one is: the
// template passes `awslogs-create-group: true` with an explicit
// `awslogs-group`, so Pulumi adds no suffix and there is nothing to
// resolve. AWS/ECS metrics are dimensioned by BOTH `ClusterName` and
// `ServiceName` (verified: CPUUtilization, MemoryUtilization and
// LiveTaskCount are listed under exactly that pair), and naming only
// the cluster would match no metric at all — the metric provider keeps
// only dimension sets whose key set EXACTLY equals the filter's.
func ecsSources(stack, region string) []TelemetrySource {
	env := envOfStack(stack)
	if env == "" {
		// Without the environment the cluster name cannot be built, and
		// a half-built one would match nothing. Returning nothing gets
		// "no telemetry handles are recorded", which is true.
		return nil
	}
	simple := stack + "-" + env
	return []TelemetrySource{{
		Kind:   KindCloudWatchLogs,
		Ref:    "/ecs/" + simple,
		Region: region,
	}, {
		Kind:   KindCloudWatchMetrics,
		Ref:    "AWS/ECS",
		Region: region,
		Dims: map[string]string{
			"ClusterName": awsResName(simple, "cluster"),
			"ServiceName": awsResName(simple, "svc"),
		},
	}}
}

// envOfStack recovers the environment from an SC stack name, which is
// `<stack>--<env>`.
//
// Returns "" when there is no `--`, which is not a parse failure to
// shrug at: every name in ecsSources is built from the environment, so
// an unsplittable stack name means no handles rather than approximate
// ones.
func envOfStack(stack string) string {
	if i := strings.LastIndex(stack, "--"); i >= 0 && i+2 < len(stack) {
		return stack[i+2:]
	}
	return ""
}

// awsResName mirrors sc-plugin-aws's own helper
// (plugin/internal/ecs_fargate.go:1287) verbatim. Copied rather than
// imported: this service does not depend on the AWS plugin, and the
// one-line rule is better duplicated with its source cited than pulled
// in behind a module boundary.
func awsResName(name, suffix string) string {
	return strings.ReplaceAll(name+"-"+suffix, "--", "_")
}

// WithDerivedSources returns rec with its telemetry handles re-derived
// from naming conventions, when a derivation is what it had.
//
// Three cases, and the distinction is the whole point:
//
//   - Inferred handles are REPLACED. They were a guess, and this build's
//     guess is the better one by definition — it is the one whose
//     conventions have been checked most recently.
//   - Published handles (Inferred == false, non-empty) are left alone.
//     A template plugin that named its own log group is authoritative;
//     overwriting it with a convention would be the service second-
//     guessing the resource's owner.
//   - No handles at all get a derivation, flagged as inferred. Cheap,
//     and it covers a record written before the plugin derived anything.
func WithDerivedSources(rec ServiceRecord) ServiceRecord {
	if len(rec.Sources) > 0 && !rec.Inferred {
		return rec
	}
	derived := DeriveSources(rec.Stack, rec.Template, regionFor(rec))
	if len(derived) == 0 {
		// Keep whatever is stored rather than blanking it: an
		// underivable template here means this build knows less than the
		// registration did, not more.
		return rec
	}
	rec.Sources = derived
	rec.Inferred = true
	return rec
}

// regionFor picks the region a derived handle should name. Empty is a
// valid answer — the provider then applies its own default region,
// which is where a single-region deployment's answer comes from.
func regionFor(rec ServiceRecord) string {
	if len(rec.Regions) > 0 {
		return rec.Regions[0]
	}
	return ""
}
