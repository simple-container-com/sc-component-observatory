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
