// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/simple-container-com/api/pkg/api"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"

	observatoryApi "github.com/simple-container-com/sc-component-observatory/api"
)

// schema is the descriptor every assertion below reads. Schema() takes no
// state off the plugin value, so a zero one is enough.
func schema(t *testing.T) *scpluginv1.SchemaResponse {
	t.Helper()
	resp, err := (&observatoryPlugin{}).Schema(context.Background(), &scpluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema() error: %v", err)
	}
	return resp
}

// TestComponentSecretEnvMapKeysAreWritable guards the "both halves are
// needed" trap that cost sc-component-backup a deploy cycle: a config field
// bound in ComponentSecretEnvMap is useless unless an operator is also
// allowed to WRITE that key, and the two lists live apart.
//
// A key is writable if EITHER core treats it as well-known
// (api.ComponentConfigTopLevelKeys — `signingKey` is there, which is why it
// is absent from this plugin's allowlist and still works) OR it is in this
// plugin's per-type ComponentAllowedOverrides. Anything else is a
// parse-time rejection of the WHOLE STACK with "unknown or disallowed
// top-level key" — and the error names the stack, not the missing entry.
//
// Written as an invariant over every entry rather than a check of the
// current three, so the next secret-bound field is covered the day it is
// added and not the day it breaks a provision.
func TestComponentSecretEnvMapKeysAreWritable(t *testing.T) {
	resp := schema(t)

	for resourceType, secretEnv := range resp.ComponentSecretEnvMap {
		allowedSet := map[string]bool{}
		if allowed := resp.ComponentAllowedOverrides[resourceType]; allowed != nil {
			for _, v := range allowed.Values {
				allowedSet[v] = true
			}
		}
		for field, envVar := range secretEnv.Entries {
			if allowedSet[field] || api.IsComponentConfigTopLevelKey(field) {
				continue
			}
			t.Errorf("%s: config field %q is bound to env %q by ComponentSecretEnvMap but is neither a core "+
				"well-known key nor in ComponentAllowedOverrides — an operator writing it would fail "+
				"parse-time validation for the entire stack", resourceType, field, envVar)
		}
	}
}

// TestMigrationTokenIsSecretBound pins the specific wire the S3 → go-embedb
// cutover depends on.
//
// Both halves matter and they fail differently. Without the allowlist entry
// the stack does not parse. Without the ComponentSecretEnvMap entry the
// token would be carried as PLAIN env instead of a per-function
// SecretsManager entry — a deploy that works, with the credential that can
// rewrite the whole datastore readable from the Lambda's environment. The
// second failure is silent, which is why it is asserted rather than left to
// review.
//
// The env var name is the contract with service/cmd/observatory, which
// reads ADMIN_MIGRATION_TOKEN and resolves it through resolveAWSSecretARN;
// renaming either side alone mounts no migration route at all.
func TestMigrationTokenIsSecretBound(t *testing.T) {
	resp := schema(t)

	secretEnv := resp.ComponentSecretEnvMap[observatoryApi.ResourceType]
	if secretEnv == nil {
		t.Fatalf("no ComponentSecretEnvMap for %q", observatoryApi.ResourceType)
	}
	if got, want := secretEnv.Entries["migrationToken"], "ADMIN_MIGRATION_TOKEN"; got != want {
		t.Errorf("migrationToken binds to env %q, want %q", got, want)
	}

	allowed := resp.ComponentAllowedOverrides[observatoryApi.ResourceType]
	if allowed == nil {
		t.Fatalf("no ComponentAllowedOverrides for %q", observatoryApi.ResourceType)
	}
	found := false
	for _, v := range allowed.Values {
		if v == "migrationToken" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("migrationToken missing from ComponentAllowedOverrides %v", allowed.Values)
	}
}

// TestSecretEnvVarNamesAreSane catches a copy-paste that would make two
// config fields fight over one env var, or bind one to the empty string.
// Mirror of notifier's schema_test.go port.
func TestSecretEnvVarNamesAreSane(t *testing.T) {
	resp := schema(t)
	envVarRe := regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	for resourceType, secretEnv := range resp.ComponentSecretEnvMap {
		if secretEnv == nil {
			continue
		}
		seen := map[string]string{}
		for cfgKey, envVar := range secretEnv.Entries {
			if envVar == "" {
				t.Errorf("secret env map: config key %q (resource %q) binds to an empty env var name", cfgKey, resourceType)
				continue
			}
			if !envVarRe.MatchString(envVar) {
				t.Errorf("secret env map: env var %q (from %q, resource %q) is not an upper-snake name", envVar, cfgKey, resourceType)
			}
			if prev, dup := seen[envVar]; dup {
				t.Fatalf("secret env map: env var %q is bound from both %q and %q (resource %q) — one would silently win", envVar, prev, cfgKey, resourceType)
			}
			seen[envVar] = cfgKey
		}
	}
}

// TestSecretEnvVarsAreResolvedByTheService is the SERVICE half of the
// secret-env wire, and it reads source rather than calling code because
// the binding lives in the plugin module and the reader lives in the
// service module — no ordinary test spans them. A grep is coarse, but
// the failure it prevents is the one that has actually happened in this
// fleet: a bound secret the service never passes through
// resolveAWSSecretARN, so the service compares against the
// SecretsManager ARN string instead of the secret and nothing errors.
//
// Mirror of notifier's schema_test.go — ported here per the sc-components
// handoff after notifier's plugin #4 landed the shape. Ported to backup
// in sc-component-backup#41 (which surfaced one live defect:
// notifyToken bound without a service-side resolver).
func TestSecretEnvVarsAreResolvedByTheService(t *testing.T) {
	resp := schema(t)
	secretEnv := resp.ComponentSecretEnvMap[observatoryApi.ResourceType]
	if secretEnv == nil || len(secretEnv.Entries) == 0 {
		t.Fatalf("Schema advertised no secret-env entries for %q — add one before removing this test", observatoryApi.ResourceType)
	}

	path := filepath.Join("..", "..", "..", "service", "cmd", "observatory", "main.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s — if the service module moved, move this assertion with it rather than deleting it: %v", path, err)
	}

	resolved := map[string]bool{}
	re := regexp.MustCompile(`resolveAWSSecretARN\([^,]+,\s*"([A-Z][A-Z0-9_]*)"`)
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		resolved[m[1]] = true
	}
	if len(resolved) == 0 {
		t.Fatalf("found no resolveAWSSecretARN call sites in %s — service wiring moved or regressed", path)
	}

	var missing []string
	for cfgKey, envVar := range secretEnv.Entries {
		if !resolved[envVar] {
			missing = append(missing, cfgKey+" → "+envVar)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s never passes these bound secrets through resolveAWSSecretARN, so the service compares against the SecretsManager ARN instead of the secret: %s", path, strings.Join(missing, ", "))
	}
}
