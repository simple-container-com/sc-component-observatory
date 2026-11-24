// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// The ARN-exit calibration, end to end: boot with a SecretsManager ARN
// the service CANNOT reach (bogus account + non-existent secret), and
// assert the binary exits 2 rather than serving.
//
// This mirrors sc-component-backup#46's calibration. Observatory
// hasn't had the exact ARN-read-by-nothing bug ship, but it shares
// the shape — `resolveAWSSecretARN` at startup, and a service that
// silently serves if resolution fails is the exact failure mode a
// handler test cannot see. Same selection-bias correction from
// sc-components applies: a bare exit-code assertion gets confused
// by secondary guards (`len(jwtSecret) < 32` fires on the empty
// resolved value if the first error-return is bypassed), so the
// test also asserts the resolve-error log line IS present AND the
// fallthrough-guard line is NOT.
//
// Observatory's shape differs from backup slightly: resolve-errors
// are returned from `run()` to `main()`, which does the `os.Exit(2)`.
// So the mutation target is each `return fmt.Errorf("resolve …")`
// line, not an inline `os.Exit(2)`. Same test contract either way.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const bogusARN = "arn:aws:secretsmanager:eu-west-1:000000000000:secret:nope-AAAAAA"

func TestBinaryExitsTwoOnServiceJWTSecretArnResolveFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary; skipped under -short")
	}
	binPath := buildBinary(t)
	assertExitsTwoOnResolveFailure(
		t, binPath, map[string]string{
			"SERVICE_JWT_SECRET":            bogusARN,
			"OBSERVATORY_OPERATOR_PASSWORD": "placeholder-not-an-arn",
			"STORAGE_TYPE":                  "filesystem",
		},
		"resolve SERVICE_JWT_SECRET",
		"SERVICE_JWT_SECRET must be at least 32 bytes",
	)
}

func TestBinaryExitsTwoOnOperatorPasswordArnResolveFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary; skipped under -short")
	}
	binPath := buildBinary(t)
	assertExitsTwoOnResolveFailure(
		t, binPath, map[string]string{
			"SERVICE_JWT_SECRET":            "0123456789abcdef0123456789abcdef",
			"OBSERVATORY_OPERATOR_PASSWORD": bogusARN,
			"STORAGE_TYPE":                  "filesystem",
		},
		"resolve OBSERVATORY_OPERATOR_PASSWORD",
		"starting sc-component-observatory",
	)
}

func buildBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "observatory")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, ".")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return binPath
}

func assertExitsTwoOnResolveFailure(t *testing.T, binPath string, extraEnv map[string]string, wantMsg, antiMsg string) {
	t.Helper()
	storeDir := t.TempDir()
	var out bytes.Buffer
	proc := exec.Command(binPath)
	env := append(
		os.Environ(),
		"PORT=0",
		"STORAGE_PATH="+storeDir,
		"COOKIE_INSECURE=1",
		"USE_GOEMBEDB=0",
		"LOCAL_DEBUG=true",
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_REGION=",
		"AWS_DEFAULT_REGION=",
	)
	for k, v := range extraEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	proc.Env = env
	proc.Stdout = &out
	proc.Stderr = &out
	if err := proc.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()

	var exitCode int
	select {
	case err := <-done:
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("binary exited without an ExitError: %v (want exit code 2)\nstderr:\n%s", err, out.String())
		}
		exitCode = exit.ExitCode()
	case <-time.After(45 * time.Second):
		_ = proc.Process.Kill()
		t.Fatalf("binary did not exit within 45s on bogus ARN\nstderr:\n%s", out.String())
	}

	stderr := out.String()
	if exitCode != 2 {
		t.Errorf("binary exit code = %d, want 2\nstderr:\n%s", exitCode, stderr)
	}
	if !strings.Contains(stderr, wantMsg) {
		t.Errorf("stderr does not contain %q — resolve-error log renamed or path bypassed\nstderr:\n%s", wantMsg, stderr)
	}
	if strings.Contains(stderr, antiMsg) {
		t.Errorf("stderr contains %q — SECONDARY guard fired; the resolve-error return was BYPASSED. Bare exit-code assertion would have been vacuous.\nstderr:\n%s", antiMsg, stderr)
	}
}
