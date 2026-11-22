// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeCompose drops a compose file in a temp dir and returns a client
// descriptor shaped the way api packs one for a `cloud-compose` stack:
// the compose file as an ABSOLUTE path (placeholders.Resolve has
// already expanded `${git:root}`), nested under the wrapped
// `Config.config`.
func writeCompose(t *testing.T, body string) map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker-compose.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	return map[string]any{
		"template": "ecs-fargate",
		"type":     "cloud-compose",
		"Config": map[string]any{"config": map[string]any{
			"domain":            "baas.simple-forge.com",
			"dockerComposeFile": path,
		}},
	}
}

// TestComposeHealthPath_ReadsTheIngressLabel is the case the two ECS
// tenants actually hit. forge-baas serves `/api/status`, says so in the
// label SC defines for it, and says it NOWHERE the client descriptor
// can see — so without this read the prober falls through its default
// chain and the recorded path is a guess.
func TestComposeHealthPath_ReadsTheIngressLabel(t *testing.T) {
	cfg := writeCompose(t, `
services:
  baas:
    image: baas:latest
    ports: ["8090:8090"]
    labels:
      "simple-container.com/ingress": "true"
      "simple-container.com/healthcheck/path": "/api/status"
    healthcheck:
      test: ["CMD", "curl", "--silent", "--fail", "http://localhost:8090/api/status"]
`)
	if got := ComposeHealthPath(context.Background(), cfg); got != "/api/status" {
		t.Errorf("ComposeHealthPath = %q, want /api/status", got)
	}
	// And through the public entry point, which is what buildRecord calls.
	if got := HealthPathFromClientConfig(context.Background(), cfg); got != "/api/status" {
		t.Errorf("HealthPathFromClientConfig = %q, want /api/status", got)
	}
}

// TestComposeHealthPath_LabelWinsOverTheProbeCommand — the label is the
// explicit declaration and it is what SC hands the load balancer, so it
// must not be shadowed by a `healthcheck.test` that probes somewhere
// else (a container can legitimately self-check an internal endpoint
// while publishing a different one).
func TestComposeHealthPath_LabelWinsOverTheProbeCommand(t *testing.T) {
	cfg := writeCompose(t, `
services:
  app:
    image: app:latest
    labels:
      "simple-container.com/ingress": "true"
      "simple-container.com/healthcheck/path": "/api/status"
    healthcheck:
      test: ["CMD-SHELL", "curl -f http://localhost:9000/internal/ping"]
`)
	if got := ComposeHealthPath(context.Background(), cfg); got != "/api/status" {
		t.Errorf("got %q, want the label to win", got)
	}
}

// TestComposeHealthPath_FallsBackToTheProbeCommand — a compose file
// with a probe but no label still states a path, in the command.
func TestComposeHealthPath_FallsBackToTheProbeCommand(t *testing.T) {
	cfg := writeCompose(t, `
services:
  app:
    image: app:latest
    labels:
      "simple-container.com/ingress": "true"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/healthz"]
`)
	if got := ComposeHealthPath(context.Background(), cfg); got != "/healthz" {
		t.Errorf("got %q, want /healthz from the probe command", got)
	}
}

// TestComposeHealthPath_SingleServiceNeedsNoIngressLabel — with one
// service there is nothing to disambiguate, so requiring the label
// would discard a path that is unambiguously the service's own.
func TestComposeHealthPath_SingleServiceNeedsNoIngressLabel(t *testing.T) {
	cfg := writeCompose(t, `
services:
  only:
    image: app:latest
    labels:
      "simple-container.com/healthcheck/path": "ready"
`)
	// Also covers normalisation: the label may omit the leading slash.
	if got := ComposeHealthPath(context.Background(), cfg); got != "/ready" {
		t.Errorf("got %q, want /ready", got)
	}
}

// TestComposeHealthPath_AmbiguousServicesInferNothing — several
// unlabelled services have no "the" health path, and picking one would
// report a healthy service as down about as often as not. SC's own ECS
// converter errors here; this component degrades to the prober's
// defaults instead, since a missing dashboard field must never fail a
// deploy.
func TestComposeHealthPath_AmbiguousServicesInferNothing(t *testing.T) {
	cfg := writeCompose(t, `
services:
  app:
    image: app:latest
    labels:
      "simple-container.com/healthcheck/path": "/api/status"
  worker:
    image: app:latest
    command: ["worker"]
`)
	if got := ComposeHealthPath(context.Background(), cfg); got != "" {
		t.Errorf("got %q from an ambiguous compose file, want no answer", got)
	}
}

// TestComposeHealthPath_MissingOrUnreadableIsSilent. Every one of these
// degrades to "" rather than erroring: this runs inside a consumer's
// deploy, and a dashboard that cannot work out a probe path must not be
// able to break it.
func TestComposeHealthPath_MissingOrUnreadableIsSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  map[string]any
	}{
		{"no descriptor", nil},
		{"no dockerComposeFile", map[string]any{"template": "lambda"}},
		{
			// A relative path cannot be resolved from here — the plugin is
			// not told the consumer's stack directory — so it is refused
			// rather than joined against whatever cwd happens to be.
			name: "relative path",
			cfg:  map[string]any{"dockerComposeFile": "docker-compose.yaml"},
		},
		{
			name: "file does not exist",
			cfg:  map[string]any{"dockerComposeFile": "/nonexistent/docker-compose.yaml"},
		},
		{
			name: "file is not compose yaml",
			cfg:  writeCompose(t, "this: is not: valid: yaml: at all\n\t- nope\n"),
		},
	} {
		if got := ComposeHealthPath(context.Background(), tc.cfg); got != "" {
			t.Errorf("%s: got %q, want no answer", tc.name, got)
		}
	}
}
