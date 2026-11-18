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
