// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package health probes registered services and records the outcome.
//
// Deliberately cloud-agnostic. The probe is an HTTP GET against the
// endpoint each service reported at registration, which works
// identically for an AWS Lambda Function URL, a Yandex container URL, a
// Kubernetes ingress and a plain VM. A per-cloud health API would mean
// a new integration per target before a new target could be seen at
// all — the exact coupling this component exists to remove.
//
// What IS cloud-specific is which regions a service lives in, and that
// is data on the record rather than code here.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
)

// Prober probes one service endpoint.
//
// An interface so a future target whose health genuinely cannot be
// reached over HTTP (a queue worker with no listener, say) can supply
// its own without the sweep logic changing.
type Prober interface {
	Probe(ctx context.Context, url string) registry.HealthResult
}

// HTTPProber is the default: a GET, following redirects, with any
// 2xx/3xx counted as up.
//
// 4xx is DOWN rather than up-with-a-caveat: a health endpoint answering
// 401 or 404 is not serving its purpose, and treating it as healthy
// would mask a broken deploy behind a green tick. A 401 in particular
// is the signature of a service whose auth middleware is mounted over
// its own health path, which is a real misconfiguration worth seeing.
type HTTPProber struct {
	Client *http.Client
	// UserAgent identifies the prober in the target's access log, so an
	// operator reading their own logs can tell observatory's traffic
	// from real users.
	UserAgent string
}

func NewHTTPProber(timeout time.Duration) *HTTPProber {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPProber{
		Client:    &http.Client{Timeout: timeout},
		UserAgent: "sc-component-observatory/healthcheck",
	}
}

// Probe makes one reading, retrying ONCE on a transport failure.
//
// A serverless cold start is not an outage. A container-image Lambda
// that has just been deployed — or has simply been idle — can take
// several seconds to answer its first request, which the 5s default
// timeout records as DOWN. That lands at the worst possible moment:
// immediately after a deploy, which is exactly when an operator is
// looking at the dashboard. Live-caught 2026-11-19 on the first tenant:
// `status: down, latency_ms: 5000` seconds after a successful deploy,
// while curl against the same URL answered 200 in 0.56s, and the next
// sweep read 58ms.
//
// So a transport failure gets one retry. The first attempt is what
// warms the target; the second is the reading. An HTTP STATUS is never
// retried — the service answered, and a 500 twice is the same 500.
//
// The cost is bounded: one extra request per genuinely-down service per
// sweep. The alternative — simply raising the timeout — would make
// every sweep wait longer on real outages without ever distinguishing
// "slow to wake" from "not there".
func (p *HTTPProber) Probe(ctx context.Context, url string) registry.HealthResult {
	res := p.probeOnce(ctx, url)
	if res.Status != registry.HealthDown || res.HTTPStatus != 0 {
		// Either healthy, or the service answered with a status code.
		return res
	}
	// Transport failure. Give the target a moment to finish waking,
	// then take the real reading.
	select {
	case <-ctx.Done():
		return res
	case <-time.After(coldStartGrace):
	}
	retry := p.probeOnce(ctx, url)
	retry.Retried = true
	if retry.Status == registry.HealthUp {
		return retry
	}
	// Still down. Keep the retry's reading but carry both errors, so a
	// log shows it was tried twice rather than looking like one flaky
	// probe.
	if retry.Error != "" && res.Error != "" && retry.Error != res.Error {
		retry.Error = truncate(res.Error+" (retry: "+retry.Error+")", 300)
	}
	return retry
}

// coldStartGrace is the pause before the retry.
//
// Short on purpose: it is a yield, not a backoff. The first attempt
// already spent the full timeout, which is most of what a cold start
// needs; this just avoids hammering the same instant.
const coldStartGrace = 500 * time.Millisecond

func (p *HTTPProber) probeOnce(ctx context.Context, url string) registry.HealthResult {
	res := registry.HealthResult{URL: url, At: time.Now().UTC()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		res.Status = registry.HealthUnknown
		res.Error = truncate(err.Error(), 300)
		return res
	}
	req.Header.Set("User-Agent", p.UserAgent)

	start := time.Now()
	resp, err := p.Client.Do(req)
	res.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		// A transport failure is DOWN, not unknown: we reached for the
		// service and could not get there, which is what an operator
		// means by down. `unknown` is reserved for "we never tried".
		res.Status = registry.HealthDown
		res.Error = truncate(err.Error(), 300)
		return res
	}
	defer resp.Body.Close()
	res.HTTPStatus = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		res.Status = registry.HealthUp
		res.Version, res.Commit = versionFromProbeBody(resp.Body)
	} else {
		res.Status = registry.HealthDown
		res.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return res
}

// maxHealthBody caps how much of a probe response is read.
//
// A health endpoint answers in bytes; anything larger is a service
// serving its SPA shell (or an error page) on the health path, and
// reading it in full would mean an unbounded read per probe per sweep.
const maxHealthBody = 8 << 10

// versionFromProbeBody reads the version and commit a service reports
// about itself.
//
// Entirely best-effort and deliberately quiet. A service that answers
// 200 with an empty body, with HTML, or with JSON in some other shape
// is HEALTHY — that is what the status code said — and must not be
// downgraded or logged as an error because its body did not match a
// convention it never agreed to.
//
// The convention it looks for is the one the fleet's own services
// follow:
//
//	{"status":"ok","service":"forge-storage","version":"2026.11.19-abc1234"}
//
// Several spellings are accepted because they already exist in the
// wild: `version`/`Version`, `commit`/`Commit`/`sha`/`revision`. A
// non-string value (a number, an object) is skipped rather than
// stringified, since a coerced value reads like a real version on the
// dashboard.
func versionFromProbeBody(body io.Reader) (version, commit string) {
	raw, err := io.ReadAll(io.LimitReader(body, maxHealthBody))
	if err != nil || len(raw) == 0 {
		return "", ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return truncate(strings.TrimSpace(v), 120)
			}
		}
		return ""
	}
	return pick("version", "Version"), pick("commit", "Commit", "sha", "revision")
}

// SweepConfig tunes one pass.
type SweepConfig struct {
	// DefaultPath is appended to an endpoint when the service declares
	// no path of its own.
	DefaultPath string

	// Concurrency caps simultaneous probes. A fleet sweep should not
	// open one connection per service at once from a Lambda with a
	// small file-descriptor budget.
	Concurrency int

	// Now is injected for tests.
	Now func() time.Time
}

// Sweep probes every registered service and records the results.
//
// Returns the results it recorded. Errors on individual services are
// recorded AS results (status down, error set) rather than returned:
// the point of a sweep is to produce a health picture, and aborting the
// whole pass because one service is unreachable would lose the
// information about every service after it.
func Sweep(ctx context.Context, store registry.Store, prober Prober, cfg SweepConfig) ([]registry.HealthResult, error) {
	if cfg.DefaultPath == "" {
		cfg.DefaultPath = "/healthz"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}

	records, err := store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}

	type job struct {
		stack  string
		region string
		url    string
	}
	var jobs []job
	for _, rec := range records {
		if rec.EndpointURL == "" {
			// Nothing to probe. Recorded as unknown so the dashboard
			// distinguishes "no endpoint published" from "down" — the
			// first is a wiring gap, the second an outage.
			jobs = append(jobs, job{stack: rec.Stack, region: "", url: ""})
			continue
		}
		url := JoinHealthURL(rec.EndpointURL, firstNonEmpty(rec.HealthPath, cfg.DefaultPath))
		if len(rec.Regions) == 0 {
			jobs = append(jobs, job{stack: rec.Stack, url: url})
			continue
		}
		for _, region := range rec.Regions {
			jobs = append(jobs, job{stack: rec.Stack, region: region, url: url})
		}
	}

	var (
		mu      sync.Mutex
		results = make([]registry.HealthResult, 0, len(jobs))
		sem     = make(chan struct{}, cfg.Concurrency)
		wg      sync.WaitGroup
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var res registry.HealthResult
			if j.url == "" {
				res = registry.HealthResult{
					Status: registry.HealthUnknown,
					At:     cfg.Now(),
					Error:  "service published no endpoint URL at registration",
				}
			} else {
				res = prober.Probe(ctx, j.url)
			}
			res.Stack = j.stack
			res.Region = j.region
			if res.At.IsZero() {
				res.At = cfg.Now()
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}(j)
	}
	wg.Wait()

	for _, res := range results {
		// Best-effort persist: a storage hiccup on one result must not
		// discard the rest of the sweep.
		_ = store.PutHealth(ctx, res)
	}
	return results, nil
}

// JoinHealthURL joins an endpoint base and a health path without
// doubling or dropping the separator.
//
// Endpoints arrive in both shapes — a Lambda Function URL always has a
// trailing slash, an ingress hostname usually does not — so this cannot
// assume either.
func JoinHealthURL(endpoint, path string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	path = strings.TrimSpace(path)
	if path == "" {
		return endpoint
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return endpoint + path
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
