// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"context"
	"strings"

	"github.com/compose-spec/compose-go/types"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/clouds/compose"
)

// ComposeHealthPath reads a `cloud-compose` consumer's own health path
// out of its docker-compose file — the way SC itself does.
//
// # Why a file read, and why here
//
// A compose stack does NOT declare its health path in client.yaml. It
// declares it in the compose file, on the ingress container, as the
// label SC defines for exactly this purpose:
//
//	services:
//	  baas:
//	    labels:
//	      "simple-container.com/ingress": "true"
//	      "simple-container.com/healthcheck/path": "/api/status"
//
// sc-plugin-aws reads that label to build the ALB target group's health
// check (`EcsFargateProbe.FromHealthCheck`,
// sc-plugin-aws/api/ecs_fargate.go:494, keyed on
// `api.ComposeLabelHealthcheckPath`). It is the service's own statement
// of where its probe lives, and it is already the thing the load
// balancer believes.
//
// The client descriptor api packs for this plugin carries only
// `dockerComposeFile` — a path, not the parsed tree — so the label is
// not reachable from the descriptor alone. It IS reachable from the
// file, because:
//
//   - `${git:root}` and friends are resolved before the descriptor is
//     packed (placeholders.Resolve rewrites `stack.Client.Stacks[env]`
//     in place), so the path that arrives here is absolute; and
//   - this runs as a deploy-time sidecar on the machine running
//     `sc deploy`, i.e. in the consumer's own checkout, so the file is
//     on disk.
//
// Both facts are why the read happens in the PLUGIN and the answer is
// PUBLISHED on the registration: the observatory service runs
// elsewhere, later, with no checkout, and could never do this itself.
//
// Failure is always "", never an error. A health path that cannot be
// read means the prober falls back to its defaults — the same place it
// was before this function existed. Breaking a deploy because a
// dashboard could not guess a probe path would be the wrong trade (see
// Process).
func ComposeHealthPath(ctx context.Context, cfg map[string]any) string {
	file := findStringDeep(cfg, "dockerComposeFile")
	if file == "" {
		return ""
	}
	// workingDir is ignored for an absolute path — ReadDockerCompose
	// derives it from the file itself (pkg/clouds/compose/compose.go:22),
	// which is what makes passing "" correct here rather than lazy. A
	// RELATIVE path would resolve against the wrong directory, so it is
	// rejected below instead of being joined against a guess.
	if !strings.HasPrefix(file, "/") {
		return ""
	}
	composeCfg, err := compose.ReadDockerCompose(ctx, "", file)
	if err != nil || composeCfg.Project == nil {
		return ""
	}
	svc, ok := ingressService(composeCfg.Project.Services)
	if !ok {
		return ""
	}
	return composeServiceHealthPath(svc)
}

// ingressService picks the container whose probe is the service's
// probe.
//
// SC's rule is a single container labelled
// `simple-container.com/ingress: "true"` — the ECS converter REQUIRES
// exactly one and errors otherwise (ecs_fargate.go:347-357). A compose
// file with several services (an app plus a sidecar worker) would
// otherwise have no well-defined "the" health path, and picking the
// first would be a coin flip that reports a healthy service as down
// roughly half the time.
//
// One deliberate softening: a single-service compose file with no
// ingress label still answers, because there is nothing to be
// ambiguous about. Anything else returns false, and the caller degrades
// to the prober's defaults rather than guessing.
func ingressService(svcs types.Services) (types.ServiceConfig, bool) {
	var marked []types.ServiceConfig
	for _, s := range svcs {
		if s.Labels[api.ComposeLabelIngressContainer] == "true" {
			marked = append(marked, s)
		}
	}
	if len(marked) == 1 {
		return marked[0], true
	}
	if len(marked) == 0 && len(svcs) == 1 {
		return svcs[0], true
	}
	return types.ServiceConfig{}, false
}

// composeServiceHealthPath extracts the HTTP path a compose service
// says it is healthy at.
//
// Label first, because it is the explicit statement and it is what SC
// hands the load balancer. Then the `healthcheck.test` command, parsed
// for a URL — forge-baas, for instance, declares both:
//
//	labels: {"simple-container.com/healthcheck/path": "/api/status"}
//	healthcheck:
//	  test: ["CMD", "curl", "--silent", "--fail",
//	         "http://localhost:8090/api/status"]
//
// One intentional divergence from sc-plugin-aws: it reads the labels
// only INSIDE `if check != nil`, so a label without a `healthcheck:`
// block is ignored there. Here the label is honoured on its own. The
// label is an unambiguous declaration of where the probe lives, and the
// consequence of the two differing is only that the dashboard knows a
// path the ALB does not — strictly more information, and never a wrong
// one.
func composeServiceHealthPath(svc types.ServiceConfig) string {
	if p := svc.Labels[api.ComposeLabelHealthcheckPath]; strings.TrimSpace(p) != "" {
		return normalizePath(p)
	}
	if svc.HealthCheck == nil || len(svc.HealthCheck.Test) == 0 {
		return ""
	}
	// pathFromComposeTest takes the []any shape the descriptor search
	// yields; the typed compose vector is converted rather than
	// duplicating the parser, so the two paths cannot drift.
	test := make([]any, 0, len(svc.HealthCheck.Test))
	for _, part := range svc.HealthCheck.Test {
		test = append(test, part)
	}
	return pathFromComposeTest(test)
}
