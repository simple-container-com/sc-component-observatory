// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package main

import (
	"context"
	"testing"
)

// TestResolveAWSSecretARN_Passthrough covers the branch every local run,
// every `dev/` compose smoke and every non-AWS deployment takes: a value
// that is not a SecretsManager ARN is returned verbatim, with no AWS client
// constructed and no network call attempted.
//
// Asserted with an ALREADY-CANCELLED context, which is what makes the
// companion test below meaningful: if the prefix check were ever loosened
// or dropped, these inputs would start taking the resolve path and the
// cancelled context would surface it as an error instead of silently
// turning a working local deploy into an AWS dependency.
func TestResolveAWSSecretARN_Passthrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"plain token", "local-throwaway-migration-token"},
		{"64-char hex key", "2f1a9c0e4b7d8f3a5e6c1b2d4f7a8c9e0b1d2f3a4c5e6b7d8f9a0c1e2d3f4a5b"},
		{"a different ARN service is not ours to resolve", "arn:aws:s3:::some-bucket"},
		{"secretsmanager appearing mid-string, not as the prefix", "see arn:aws:secretsmanager:eu-central-1:1:secret:x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveAWSSecretARN(ctx, "TEST_VAR", tc.raw)
			if err != nil {
				t.Fatalf("unexpected error for %q: %v — the value took the AWS resolve path", tc.raw, err)
			}
			if got != tc.raw {
				t.Errorf("got %q, want the input verbatim %q", got, tc.raw)
			}
		})
	}
}

// TestResolveAWSSecretARN_ARNTakesTheResolvePath proves an
// `arn:aws:secretsmanager:` value is NOT returned verbatim.
//
// This is the failure that cost two live deploys (see the comment block on
// resolveAWSSecretARN): the service compared an operator-typed password,
// and separately validated a plugin-minted JWT, against the 115-char ARN
// string. Both looked perfectly configured and failed every request.
//
// Hermetic: the context is already cancelled, so the SDK call cannot
// succeed or reach the network. An error here is the PASS condition — it
// can only be reached by having entered the resolve branch at all.
func TestResolveAWSSecretARN_ARNTakesTheResolvePath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	const arn = "arn:aws:secretsmanager:eu-central-1:123456789012:secret:observatory-migration-token-AbCdEf"

	got, err := resolveAWSSecretARN(ctx, "ADMIN_MIGRATION_TOKEN", arn)
	if err == nil && got == arn {
		t.Fatalf("an ARN was returned verbatim — the prefix check is not routing it to SecretsManager, so "+
			"the service would authenticate callers against the ARN string: %q", got)
	}
}
