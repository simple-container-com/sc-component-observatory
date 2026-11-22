// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package configdiff

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// wrapped builds the shape the plugin actually captures: api packs
// `stack.Client.Stacks[env]`, so what lands in the snapshot is
// {"Config": {"config": {…}}} — a capitalized embedded Go field with no
// json tag wrapping a lowercase one. Every test uses it, because a
// differ that only works on a flat map works on nothing real. (The
// endpoint-URL extractor shipped once against the flat shape and
// silently produced "" in production.)
func wrapped(cfg map[string]any) map[string]any {
	return map[string]any{
		"template": "lambda",
		"type":     "single-image",
		"Config":   map[string]any{"config": cfg},
	}
}

func find(changes []Change, key string) (Change, bool) {
	for _, c := range changes {
		if c.Key == key {
			return c, true
		}
	}
	return Change{}, false
}

func TestCompute_EnvChangesAreBucketedAndKeyedByName(t *testing.T) {
	prev := wrapped(map[string]any{
		"env": map[string]any{"LOG_LEVEL": "info", "GONE": "x"},
	})
	cur := wrapped(map[string]any{
		"env": map[string]any{"LOG_LEVEL": "debug", "ADDED": "y"},
	})

	d := Compute(prev, cur)
	if d.Unavailable != "" {
		t.Fatalf("unexpected unavailable: %s", d.Unavailable)
	}
	if len(d.Env) != 3 {
		t.Fatalf("expected 3 env changes, got %d: %+v", len(d.Env), d.Env)
	}

	// The display key is the variable's own name — an operator who wrote
	// LOG_LEVEL should not have to read `Config.config.env.LOG_LEVEL`.
	ch, ok := find(d.Env, "LOG_LEVEL")
	if !ok {
		t.Fatalf("LOG_LEVEL not keyed by name: %+v", d.Env)
	}
	if ch.Kind != KindChanged || ch.Previous != "info" || ch.Current != "debug" {
		t.Errorf("wrong change: %+v", ch)
	}
	if ch.Path != "Config.config.env.LOG_LEVEL" {
		t.Errorf("full path should be kept for precision, got %q", ch.Path)
	}
	if ch, _ := find(d.Env, "ADDED"); ch.Kind != KindAdded || ch.Previous != "" {
		t.Errorf("added var wrong: %+v", ch)
	}
	if ch, _ := find(d.Env, "GONE"); ch.Kind != KindRemoved || ch.Current != "" {
		t.Errorf("removed var wrong: %+v", ch)
	}
}

// TestCompute_ComputeShapeIsGenericAcrossTargets is the requirement
// that this must work for ECS / GCP / VPS later without a code change
// here. Each of these is a different cloud's name for "the deployed
// shape", and none of them is special-cased.
func TestCompute_ComputeShapeIsGenericAcrossTargets(t *testing.T) {
	for _, tc := range []struct {
		name          string
		prev, cur     map[string]any
		wantKeySuffix string
	}{
		{
			name:          "lambda memory",
			prev:          map[string]any{"maxMemory": 512},
			cur:           map[string]any{"maxMemory": 1024},
			wantKeySuffix: "maxMemory",
		},
		{
			name:          "ecs desired count",
			prev:          map[string]any{"desiredCount": 2},
			cur:           map[string]any{"desiredCount": 4},
			wantKeySuffix: "desiredCount",
		},
		{
			name:          "k8s replicas",
			prev:          map[string]any{"replicas": 3},
			cur:           map[string]any{"replicas": 1},
			wantKeySuffix: "replicas",
		},
		{
			name:          "yc scale bounds, nested",
			prev:          map[string]any{"scale": map[string]any{"min": 1, "max": 10}},
			cur:           map[string]any{"scale": map[string]any{"min": 1, "max": 20}},
			wantKeySuffix: "scale.max",
		},
		{
			name:          "k8s resource limits, nested two deep",
			prev:          map[string]any{"resources": map[string]any{"limits": map[string]any{"cpu": "500m"}}},
			cur:           map[string]any{"resources": map[string]any{"limits": map[string]any{"cpu": "1"}}},
			wantKeySuffix: "resources.limits.cpu",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Compute(wrapped(tc.prev), wrapped(tc.cur))
			if len(d.Compute) != 1 {
				t.Fatalf("expected 1 compute change, got %d (env=%d settings=%d): %+v",
					len(d.Compute), len(d.Env), len(d.Settings), d)
			}
			if d.Compute[0].Key != tc.wantKeySuffix {
				t.Errorf("key = %q, want %q", d.Compute[0].Key, tc.wantKeySuffix)
			}
		})
	}
}

// TestCompute_SecretRotationIsVisibleButNotPrinted — the reason the
// plugin fingerprints its redactions at all.
func TestCompute_SecretRotationIsVisibleButNotPrinted(t *testing.T) {
	prev := wrapped(map[string]any{"apiKey": "[redacted:aaaaaa]"})
	cur := wrapped(map[string]any{"apiKey": "[redacted:bbbbbb]"})

	d := Compute(prev, cur)
	if len(d.Settings) != 1 {
		t.Fatalf("expected the rotation to show up once, got %+v", d)
	}
	ch := d.Settings[0]
	if !ch.Secret {
		t.Error("change should be marked Secret so the UI can label it")
	}
	if ch.Previous != "[redacted]" || ch.Current != "[redacted]" {
		t.Errorf("fingerprints must not be rendered: %+v", ch)
	}

	// And an UNCHANGED secret must stay quiet, or every deploy reports a
	// rotation and the signal is worthless.
	same := Compute(prev, prev)
	if !same.Empty() {
		t.Errorf("unchanged secret produced a change: %+v", same)
	}
}

// TestCompute_MissingSnapshotIsUnavailableNotEmpty is the
// silent-degradation rule in test form: "we never recorded it" and
// "nothing changed" are different answers, and an operator acts
// differently on each.
func TestCompute_MissingSnapshotIsUnavailableNotEmpty(t *testing.T) {
	cfg := wrapped(map[string]any{"env": map[string]any{"A": "1"}})

	for _, tc := range []struct {
		name      string
		prev, cur map[string]any
	}{
		{"no previous", nil, cfg},
		{"no current", cfg, nil},
		{"neither", nil, nil},
		{"empty maps", map[string]any{}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Compute(tc.prev, tc.cur)
			if d.Unavailable == "" {
				t.Fatalf("expected a stated reason, got a %d-change diff", d.Count())
			}
			if d.Empty() {
				t.Error("Empty() must be false for an uncomputable diff")
			}
			// Non-nil slices: the SPA maps over them unguarded.
			if d.Env == nil || d.Compute == nil || d.Settings == nil || d.Artifact == nil {
				t.Error("every bucket must be a non-nil slice")
			}
		})
	}
}

// TestCompute_IdenticalConfigsAreEmptyNotUnavailable is the other half
// of the pair above.
func TestCompute_IdenticalConfigsAreEmptyNotUnavailable(t *testing.T) {
	cfg := wrapped(map[string]any{
		"env":     map[string]any{"A": "1"},
		"uses":    []any{"mongodb", "my-observatory"},
		"domain":  "x.example.com",
		"timeout": 30,
	})
	d := Compute(cfg, cfg)
	if d.Unavailable != "" {
		t.Fatalf("a computable diff must not claim unavailable: %s", d.Unavailable)
	}
	if !d.Empty() {
		t.Errorf("identical configs must diff to nothing: %+v", d)
	}
}

// TestCompute_ArtifactFieldsAreSeparated — the image tag and version
// change on EVERY deploy by construction. Bucketed rather than dropped:
// hiding them would be a small lie, and an unexpected image NAME change
// is worth seeing.
func TestCompute_ArtifactFieldsAreSeparated(t *testing.T) {
	d := Compute(
		wrapped(map[string]any{"image": "repo/x:2026.11.21-aaaaaaa", "env": map[string]any{"A": "1"}}),
		wrapped(map[string]any{"image": "repo/x:2026.11.22-bbbbbbb", "env": map[string]any{"A": "2"}}),
	)
	if len(d.Artifact) != 1 || d.Artifact[0].Key != "image" {
		t.Fatalf("image should land in Artifact: %+v", d)
	}
	if len(d.Env) != 1 {
		t.Errorf("the env change must still be reported separately: %+v", d.Env)
	}
}

// TestCompute_ScalarListIsOneValue — `uses: [a, b]` reads as a set. Two
// indexed leaves would churn the diff whenever an unrelated entry is
// inserted ahead of them.
func TestCompute_ScalarListIsOneValue(t *testing.T) {
	d := Compute(
		wrapped(map[string]any{"uses": []any{"mongodb", "my-observatory"}}),
		wrapped(map[string]any{"uses": []any{"mongodb", "my-backup", "my-observatory"}}),
	)
	if len(d.Settings) != 1 || d.Settings[0].Key != "uses" {
		t.Fatalf("expected one `uses` change, got %+v", d.Settings)
	}
	if !strings.Contains(d.Settings[0].Current, "my-backup") {
		t.Errorf("the new entry should be visible in the value: %q", d.Settings[0].Current)
	}
}

// TestCompute_NestedComposeServices — a compose stack nests per-service
// blocks, which must still diff to a precise path rather than one
// enormous blob.
func TestCompute_NestedComposeServices(t *testing.T) {
	mk := func(level string) map[string]any {
		return wrapped(map[string]any{
			"dockerComposeFile": "docker-compose.yaml",
			"runs":              []any{"api"},
			"services": map[string]any{
				"api": map[string]any{
					"environment": map[string]any{"LOG_LEVEL": level},
					"healthcheck": map[string]any{"test": []any{"CMD", "curl", "http://localhost:8080/healthz"}},
				},
			},
		})
	}
	d := Compute(mk("info"), mk("debug"))
	if len(d.Env) != 1 {
		t.Fatalf("a compose service's environment must land in Env: %+v", d)
	}
	if d.Env[0].Key != "LOG_LEVEL" {
		t.Errorf("key = %q, want LOG_LEVEL", d.Env[0].Key)
	}
	if d.Env[0].Path != "Config.config.services.api.environment.LOG_LEVEL" {
		t.Errorf("path lost the service name: %q", d.Env[0].Path)
	}
}

// TestCompute_ChangeCapIsReportedNotSilent — a bounded diff that does
// not say it is bounded is a wrong diff.
func TestCompute_ChangeCapIsReportedNotSilent(t *testing.T) {
	prev := map[string]any{}
	cur := map[string]any{}
	for i := 0; i < maxChanges+50; i++ {
		prev[fmt.Sprintf("k%04d", i)] = "a"
		cur[fmt.Sprintf("k%04d", i)] = "b"
	}
	d := Compute(wrapped(prev), wrapped(cur))
	if !d.Truncated {
		t.Error("hitting the change cap must set Truncated")
	}
	if d.Count() > maxChanges {
		t.Errorf("emitted %d changes, cap is %d", d.Count(), maxChanges)
	}
}

// TestCompute_LongValuesAreTruncatedSafely — a mid-codepoint cut puts
// invalid UTF-8 into the JSON response, which encoding/json then
// replaces silently.
func TestCompute_LongValuesAreTruncatedSafely(t *testing.T) {
	d := Compute(
		wrapped(map[string]any{"note": strings.Repeat("д", 400)}),
		wrapped(map[string]any{"note": strings.Repeat("д", 500)}),
	)
	if len(d.Settings) != 1 {
		t.Fatalf("expected one change: %+v", d)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `�`) {
		t.Error("truncation produced invalid UTF-8")
	}
	if !strings.HasSuffix(d.Settings[0].Current, "…") {
		t.Errorf("long value was not truncated: %d bytes", len(d.Settings[0].Current))
	}
}

// TestCompute_TypeChangeIsAChange — a value that becomes a map (or the
// reverse) must not be swallowed by the walker.
func TestCompute_TypeChangeIsAChange(t *testing.T) {
	d := Compute(
		wrapped(map[string]any{"storage": "observatory-data"}),
		wrapped(map[string]any{"storage": map[string]any{"name": "observatory-data", "prefix": "obs"}}),
	)
	if d.Count() == 0 {
		t.Fatalf("a scalar becoming a map must register: %+v", d)
	}
}
