// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"strings"
	"testing"
)

func TestNormalizeURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// A bare hostname must get https, not http: an SC Component
		// always sits behind TLS, and http would redirect-or-fail on
		// every registration.
		{"observatory.example.com", "https://observatory.example.com"},
		{"https://observatory.example.com", "https://observatory.example.com"},
		{"https://observatory.example.com/", "https://observatory.example.com"},
		{"http://localhost:8080", "http://localhost:8080"}, // explicit http preserved for local dev
		{"  obs.example.com  ", "https://obs.example.com"},
		{"", ""},
	} {
		if got := normalizeURL(tc.in); got != tc.want {
			t.Errorf("normalizeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSelfURLFromConfig(t *testing.T) {
	withIngress := map[string]any{
		"ingress": map[string]any{"hostname": "observatory.simple-forge.com"},
	}
	if got := SelfURLFromConfig(withIngress); got != "https://observatory.simple-forge.com" {
		t.Errorf("got %q", got)
	}
	// No ingress is legitimate — a component reachable only inside the
	// VPC still serves a registry. Inventing a URL would be worse.
	for _, raw := range []map[string]any{
		{},
		{"ingress": map[string]any{}},
		{"ingress": map[string]any{"hostname": ""}},
		{"ingress": "not-a-map"},
	} {
		if got := SelfURLFromConfig(raw); got != "" {
			t.Errorf("SelfURLFromConfig(%v) = %q, want empty", raw, got)
		}
	}
}

// TestDefaultBucketName_ObeysS3Rules — an invalid bucket name fails at
// provision time with an AWS error that does not name the component,
// which is an expensive thing to debug.
func TestDefaultBucketName_ObeysS3Rules(t *testing.T) {
	got := DefaultBucketName("my-observatory", "staging")
	if got != "sc-observatory-my-observatory-staging" {
		t.Errorf("got %q", got)
	}
	if strings.ToLower(got) != got {
		t.Error("bucket names must be lowercase")
	}

	// Underscores are illegal in S3 bucket names.
	if u := DefaultBucketName("my_observatory", "staging"); strings.Contains(u, "_") {
		t.Errorf("underscore survived: %q", u)
	}

	// 63-char cap, and no trailing hyphen after truncation.
	long := DefaultBucketName(strings.Repeat("x", 90), "staging")
	if len(long) > 63 {
		t.Errorf("exceeds the 63-char S3 limit: %d", len(long))
	}
	if strings.HasSuffix(long, "-") {
		t.Errorf("truncation left a trailing hyphen: %q", long)
	}
}

// TestReadSigningKey_ErrorNamesTheFix — a config error surfaces during
// provision, where a vague message costs an operator a slow
// round-trip. The message must say what to write.
func TestReadSigningKey_ErrorNamesTheFix(t *testing.T) {
	p := NewProcessor(ProcessorConfig{})
	_, err := p.readSigningKey(inputWithRaw(map[string]any{}))
	if err == nil {
		t.Fatal("expected an error for a missing signingKey")
	}
	if !strings.Contains(err.Error(), "config.signingKey") {
		t.Errorf("error should name the key to set, got: %v", err)
	}
}

func TestReadSigningKey_HappyPath(t *testing.T) {
	p := NewProcessor(ProcessorConfig{})
	got, err := p.readSigningKey(inputWithRaw(map[string]any{"signingKey": "s3cr3t"}))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got != "s3cr3t" {
		t.Errorf("got %q", got)
	}
}

// TestBucketForStorageRef covers the operator-facing contract the
// mandate turns on: `storage:` names another SC RESOURCE, not a bucket.
//
// The string form is the one an operator actually writes, and it was
// the one that silently did not work: the first implementation only
// looked for a `storage.bucket` map key, so `storage:
// sc-observatory-data` fell through to the component's own default
// name. The deploy was green, the preview plan created
// `sc-observatory-data--staging`, and the Lambda was handed
// `STORAGE_BUCKET=sc-observatory-my-observatory-staging` — a bucket
// that does not exist. Nothing failed until the first write.
func TestBucketForStorageRef(t *testing.T) {
	// 3. The shape an operator writes: a resource name.
	if got := BucketForStorageRef(map[string]any{
		"storage": "sc-observatory-data",
	}, "my-observatory", "staging"); got != "sc-observatory-data--staging" {
		t.Errorf("string resource ref: got %q, want sc-observatory-data--staging", got)
	}

	// 2. The same reference in map form.
	if got := BucketForStorageRef(map[string]any{
		"storage": map[string]any{"name": "sc-observatory-data"},
	}, "my-observatory", "staging"); got != "sc-observatory-data--staging" {
		t.Errorf("map name ref: got %q, want sc-observatory-data--staging", got)
	}

	// 1. An explicit bucket wins over everything — the escape hatch for
	// an s3-bucket resource carrying its own `bucketName`, which the
	// convention cannot see.
	if got := BucketForStorageRef(map[string]any{
		"storage": map[string]any{
			"name":   "sc-observatory-data",
			"bucket": "an-explicitly-named-bucket",
		},
	}, "my-observatory", "staging"); got != "an-explicitly-named-bucket" {
		t.Errorf("explicit bucket: got %q, want an-explicitly-named-bucket", got)
	}

	// 4. Absent: the component names its own, so a zero-config deploy
	// still works and `storage:` stays optional.
	if got := BucketForStorageRef(map[string]any{}, "my-observatory", "staging"); got != DefaultBucketName("my-observatory", "staging") {
		t.Errorf("absent storage: got %q, want the default", got)
	}

	// Whitespace-only must not resolve to `--staging`, a bucket name
	// with an empty resource segment. SC core's destructive-rename
	// guard exists because exactly that class of empty segment reaches
	// a live plan.
	if got := BucketForStorageRef(map[string]any{"storage": "   "}, "my-observatory", "staging"); got != DefaultBucketName("my-observatory", "staging") {
		t.Errorf("blank storage: got %q, want the default", got)
	}

	// An env-less dispatch must not append a dangling separator.
	if got := BucketForStorageRef(map[string]any{"storage": "data"}, "obs", ""); got != "data" {
		t.Errorf("empty env: got %q, want data", got)
	}
}
