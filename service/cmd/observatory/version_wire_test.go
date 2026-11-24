// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// The build stamp, end to end: ldflags -> main.version ->
// SessionConfig.Version -> the JSON an operator reads.
//
// Why a binary test and not a handler test. The handler tests in
// internal/api build their own SessionConfig, so every one of them
// passes with main.go never setting Version at all. That is the exact
// shape of the bug this repo has shipped before — a value that is
// present, correct and READ BY NOTHING, logging identically to a
// working deploy. The only honest assertion is to ask the real binary
// what build it is.
//
// Why the field exists at all: a pin bump was unverifiable from
// outside. /healthz used to carry the version plus the commit SHA and
// was narrowed to a bare "ok" when that SHA — of a private repo —
// turned out to be reachable anonymously on monitor.simple-forge.com.
// The version is logged once at boot, and reading that log needs AWS
// credentials the person smoking a deploy usually does not have — so
// "the provision went green" was the only evidence that a new image
// was serving, which is evidence that the apply ran, not that this
// process is the build you merged.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A value no default could produce: `version` falls back to "0.0.0"
// and a real build stamps CalVer, so neither can pass this by accident.
const wantVersion = "v2026.11.24-wiretest"

func TestSessionConfig_IsWiredWithTheBuildVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary; skipped under -short")
	}

	bin := filepath.Join(t.TempDir(), "observatory")
	build := exec.Command("go", "build",
		"-ldflags", "-X main.version="+wantVersion, "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	port := freePort(t)
	const password = "correct-horse-battery-staple"

	cmd := exec.Command(bin)
	cmd.Env = append(
		os.Environ(),
		// LOCAL_DEBUG=true is what makes the SDK serve ordinary HTTP
		// instead of waiting for the Lambda Runtime API. The deployed
		// shape is covered elsewhere; what matters here is the handler
		// wiring, which is identical on both branches.
		"LOCAL_DEBUG=true",
		fmt.Sprintf("PORT=%d", port),
		"SERVICE_JWT_SECRET=0123456789abcdef0123456789abcdef",
		"OBSERVATORY_OPERATOR_PASSWORD="+password,
		"STORAGE_TYPE=filesystem",
		"STORAGE_PATH="+t.TempDir(),
		// Explicitly off: this test must not depend on the go-embedb
		// branch, which needs a bucket on Lambda and is not what is
		// under test.
		"USE_GOEMBEDB=0",
		// Plain HTTP for the probe. Boot otherwise sets Secure on the
		// session cookie and a non-TLS client drops it before the
		// authed probe runs.
		"COOKIE_INSECURE=1",
	)
	var logs strings.Builder
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealthz(t, base, &logs)

	client := &http.Client{Timeout: 10 * time.Second}

	// The anonymous probe first, so a leak is caught before the cookie
	// exists rather than being masked by it.
	anon := probeSession(t, client, base, nil)
	if _, ok := anon["version"]; ok {
		t.Fatalf("the unauthenticated session probe reported the build %q — that is an exact "+
			"commit SHA of a private repo, handed to anyone who can reach the hostname. "+
			"/api/v1/session is reachable without a cookie by design (the SPA needs it before "+
			"it holds one), so the version has to stay behind `if authed`.", anon["version"])
	}

	// Log in exactly the way the operator smoke does.
	body, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
		t.Fatalf("marshal login: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/login", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/login: %v\nlogs:\n%s", err, logs.String())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /api/v1/login returned %d, want 204\nlogs:\n%s", resp.StatusCode, logs.String())
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "sc_observatory_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("login set no session cookie\nlogs:\n%s", logs.String())
	}

	authed := probeSession(t, client, base, cookie)
	got, _ := authed["version"].(string)
	if got != wantVersion {
		t.Fatalf("the authenticated session probe reported version %q, want %q.\n"+
			"The binary was built with -X main.version=%s, so either main.go stopped passing "+
			"`version` into api.SessionConfig or the handler stopped reporting it. Either way a "+
			"pin bump is unverifiable from outside again: /healthz answers a bare \"ok\" and the "+
			"boot log needs AWS credentials.\nlogs:\n%s",
			got, wantVersion, wantVersion, logs.String())
	}
}

func probeSession(t *testing.T, client *http.Client, base string, cookie *http.Cookie) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/session", nil)
	if err != nil {
		t.Fatalf("build session request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/session returned %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode session body: %v", err)
	}
	return got
}

// freePort asks the kernel for one and closes it, rather than hardcoding
// 8080 — these tests run alongside whatever else is on the box, and a
// port clash would fail as a connection error that reads like a broken
// binary.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return port
}

func waitForHealthz(t *testing.T, base string, logs *strings.Builder) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the binary never answered /healthz in 30s\nlogs:\n%s", logs.String())
}
