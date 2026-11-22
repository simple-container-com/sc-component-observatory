// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"testing"

	"github.com/simple-container-com/api/pkg/api"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
	"github.com/simple-container-com/api/pkg/plugin/runtime"
)

// collectorWith returns a real runtime.Collector carrying env as if a
// preceding `uses:` sibling had emitted it.
//
// Seeded through NewProvisionCollector — the real path — rather than via
// AddEnvVariableIfNotExist, which appends to the OUTGOING pool while
// EnvVariables() reads the INCOMING one. Writing then reading back is
// the obvious-looking way to build this fixture and it silently yields
// an empty slice, so the test would pass against a deriveVersion that
// never consulted the collector at all.
//
// A real collector rather than a fake: ComputeContextCollector is an
// 18-method interface, so a stub is both a lot of code and a thing that
// drifts out of step with what it claims to implement.
func collectorWith(env map[string]string) *runtime.Collector {
	vars := make([]*scpluginv1.ComputeEnvVariable, 0, len(env))
	for k, v := range env {
		vars = append(vars, &scpluginv1.ComputeEnvVariable{Name: k, Value: v})
	}
	return runtime.NewProvisionCollector(&scpluginv1.ProvisionRequest{
		Header:         &scpluginv1.RequestHeader{StackName: "forge-storage--staging"},
		ComputeEnvVars: vars,
	})
}

// TestDeriveVersion_PrefersBusHeader pins the precedence order.
//
// The bus header (api#640) carries the value the CI action passed as
// `--deploy-version`, verbatim, so it wins. The env fallback stays
// because it is what worked BEFORE the header existed: a consumer that
// routes VERSION through the compute context itself. If the env won
// instead, a stale or hand-set VERSION from some sibling would quietly
// override the real deploy version.
func TestDeriveVersion_PrefersBusHeader(t *testing.T) {
	col := collectorWith(map[string]string{"VERSION": "from-sibling-env"})

	p := NewProcessor(ProcessorConfig{
		DeployIdentity: runtime.DeployIdentity{Version: "2026.11.19-fecb8f8"},
	})
	if got := p.deriveVersion(api.ResourceInput{}, col); got != "2026.11.19-fecb8f8" {
		t.Errorf("deriveVersion = %q, want the header value", got)
	}

	// No header (an older SC core) → the pre-#640 env path still works.
	old := NewProcessor(ProcessorConfig{})
	if got := old.deriveVersion(api.ResourceInput{}, col); got != "from-sibling-env" {
		t.Errorf("without a header: deriveVersion = %q, want the env fallback", got)
	}

	// Nothing available → "", and the SERVICE then falls back to what
	// the deployed process reports on its own health endpoint.
	if got := old.deriveVersion(api.ResourceInput{}, collectorWith(nil)); got != "" {
		t.Errorf("with nothing available: deriveVersion = %q, want empty", got)
	}

	// StackParams is the last resort, for an in-tree dispatch that
	// populates it directly.
	if got := old.deriveVersion(api.ResourceInput{
		StackParams: &api.StackParams{Version: "2026.01.01-aaaaaaa"},
	}, collectorWith(nil)); got != "2026.01.01-aaaaaaa" {
		t.Errorf("StackParams fallback: deriveVersion = %q", got)
	}
}

// TestDeriveCommit_PrefersBusHeader — same ordering, and the env
// fallback matters more here: SC auto-injects only VERSION, so before
// api#640 a commit was available ONLY when a service opted in by
// declaring GITHUB_SHA under `image.build.args`.
func TestDeriveCommit_PrefersBusHeader(t *testing.T) {
	const full = "fecb8f8d23335414982d668d0a633a4f36dae341"
	const other = "0000000000000000000000000000000000000000"
	col := collectorWith(map[string]string{"GITHUB_SHA": other})

	p := NewProcessor(ProcessorConfig{
		DeployIdentity: runtime.DeployIdentity{Commit: full},
	})
	if got := p.deriveCommit(col); got != full {
		t.Errorf("deriveCommit = %q, want the header value", got)
	}

	old := NewProcessor(ProcessorConfig{})
	if got := old.deriveCommit(col); got != other {
		t.Errorf("without a header: deriveCommit = %q, want the env fallback", got)
	}
	if got := old.deriveCommit(collectorWith(nil)); got != "" {
		t.Errorf("with nothing available: deriveCommit = %q, want empty", got)
	}
}

// composeDescriptor is a `cloud-compose` consumer's client descriptor in
// the shape api actually packs it: the operator's own config two levels
// down, under a capitalized outer key, with `template` and `type` at the
// top. Mirrors what forge-baas sends.
func composeDescriptor() map[string]any {
	return map[string]any{
		"template": "ecs-fargate",
		"type":     "cloud-compose",
		"parent":   "forge/infra",
		"Config": map[string]any{
			"config": map[string]any{
				"domain": "baas.simple-forge.com",
				"uses":   []any{"mongodb", "my-backup", "my-observatory"},
			},
		},
	}
}

// TestDeriveTemplate_ReadsTheConsumerNotTheComponent is the regression
// test for the defect that would have made the first ECS tenant useless.
//
// deriveTemplate used to read `deployTo` out of the COMPONENT's own
// opaque config — `my-observatory`'s `deployTo: lambda`, which says
// where the observatory's own Lambda runs. Every tenant therefore
// registered as `template: lambda`. That is invisible while every
// tenant IS a Lambda (all six were) and wrong the moment an ECS stack
// registers: the read-time derivation hands a `lambda` record
// `<stack>-access-logs` and `AWS/Lambda`, and both panels resolve to
// nothing at all.
func TestDeriveTemplate_ReadsTheConsumerNotTheComponent(t *testing.T) {
	p := NewProcessor(ProcessorConfig{ClientConfig: composeDescriptor()})
	if got := p.deriveTemplate(composeDescriptor()); got != "ecs-fargate" {
		t.Errorf("deriveTemplate = %q, want the CONSUMER's template", got)
	}

	// The component's own config must not be consulted as a fallback:
	// `deployTo` describes a different stack, so a value from it is a
	// confident answer about the wrong resource. Empty degrades
	// honestly — no cloud, no handles, and the dashboard says so.
	own := &api.ResourceDescriptor{Name: "my-observatory", Type: "sc-observatory"}
	own.Config.Config = map[string]any{"deployTo": "lambda"}
	bare := NewProcessor(ProcessorConfig{})
	if got := bare.deriveTemplate(nil); got != "" {
		t.Errorf("without a client descriptor: deriveTemplate = %q, want empty", got)
	}
	if rec := bare.buildRecord("forge-baas--staging", api.ResourceInput{Descriptor: own},
		collectorWith(nil), nil, []byte("k")); rec.Template != "" || rec.Cloud != "" || len(rec.Sources) != 0 {
		t.Errorf("record built from the component's own config: %+v", rec)
	}
}

// TestBuildRecord_ECSConsumerGetsECSHandles walks the whole chain the
// ECS wiring depends on: the consumer's template is read from its
// descriptor, mapped to AWS, and turned into the three live-verified
// ECS names.
func TestBuildRecord_ECSConsumerGetsECSHandles(t *testing.T) {
	p := NewProcessor(ProcessorConfig{ClientConfig: composeDescriptor()})
	rec := p.buildRecord("forge-baas--staging", api.ResourceInput{},
		collectorWith(nil), composeDescriptor(), []byte("fp-key"))

	if rec.Template != "ecs-fargate" || rec.Cloud != CloudAWS {
		t.Fatalf("template/cloud = %q/%q", rec.Template, rec.Cloud)
	}
	if !rec.Inferred {
		t.Error("Inferred = false; derived handles must render AS a guess")
	}
	if rec.EndpointURL != "https://baas.simple-forge.com" {
		t.Errorf("EndpointURL = %q, want the consumer's own domain", rec.EndpointURL)
	}
	var logs, metrics *TelemetrySource
	for i := range rec.Sources {
		switch rec.Sources[i].Kind {
		case KindCloudWatchLogs:
			logs = &rec.Sources[i]
		case KindCloudWatchMetrics:
			metrics = &rec.Sources[i]
		}
	}
	if logs == nil || logs.Ref != "/ecs/forge-baas--staging-staging" {
		t.Errorf("log handle = %+v", logs)
	}
	if metrics == nil || metrics.Dims["ServiceName"] != "forge-baas_staging-staging-svc" {
		t.Errorf("metric handle = %+v", metrics)
	}
}
