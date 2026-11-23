// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
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
