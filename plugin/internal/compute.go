// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package observatory is the sc-observatory plugin's implementation.
package observatory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	"github.com/simple-container-com/api/pkg/plugin/runtime"
)

// Push-register retry policy. Matches sc-component-backup's, which
// matches the sc-components-platform §P3 push-model contract: a
// consumer deploy must not fail because the component service is cold
// or mid-roll, but it also must not silently skip registration.
const (
	maxAttempts    = 3
	attemptTimeout = 10 * time.Second
	jwtLifetime    = 60 * time.Second
	userAgent      = "sc-component-observatory/0.0.1 (+https://github.com/simple-container-com/sc-component-observatory)"
	registerPath   = "/api/v1/services/register"

	// agentURLVar is injected into the consumer's env. Not required by
	// the consumer itself, but it makes the wiring visible in the
	// Lambda console, which is worth a lot when debugging why a service
	// is missing from the dashboard.
	agentURLVar = "OBSERVATORY_AGENT_URL"
)

var backoffSeconds = []int{1, 2, 4}

// ProcessorConfig carries per-plugin wiring.
type ProcessorConfig struct {
	Progress   *scPlugin.Publisher
	HTTPClient *http.Client

	// ClientConfig is the consuming stack's own client descriptor,
	// decoded from ComputeProcessRequest.client_descriptor_json
	// (api#639). Nil against an older SC core — the processor degrades
	// to recording deploy facts without the consumer's config, which is
	// the pre-api#639 behaviour.
	ClientConfig map[string]any

	// DeployIdentity is what the bus says about the code being deployed:
	// version, full commit SHA, `<owner>/<repo>` (api#640).
	//
	// Separate from ClientConfig because it describes THIS deploy rather
	// than the consumer's declared configuration, and because every
	// field is independently best-effort — `sc provision` has no deploy
	// version, a deploy outside a git worktree has no SHA. Empty means
	// NOT KNOWN and must never be recorded as a value.
	//
	// Zero value against an older SC core, which is the pre-api#640
	// behaviour: version falls back to the health probe and commit to
	// whatever the CalVer string's trailing segment yields.
	DeployIdentity runtime.DeployIdentity
}

// Processor is the ComputeProcessor for a `uses: [my-observatory]`
// consumer.
type Processor struct{ cfg ProcessorConfig }

func NewProcessor(cfg ProcessorConfig) *Processor {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: attemptTimeout}
	}
	return &Processor{cfg: cfg}
}

// ServiceRecord is the push-register body. Mirrors the service's
// registerRequest; the two modules are independent so the shape is
// declared on both sides.
type ServiceRecord struct {
	Stack       string            `json:"stack"`
	Cloud       string            `json:"cloud"`
	Template    string            `json:"template"`
	Version     string            `json:"version"`
	Commit      string            `json:"commit,omitempty"`
	Repository  string            `json:"repository,omitempty"`
	EndpointURL string            `json:"endpoint_url,omitempty"`
	Inferred    bool              `json:"inferred,omitempty"`
	Sources     []TelemetrySource `json:"sources,omitempty"`
	Regions     []string          `json:"regions,omitempty"`
	HealthPath  string            `json:"health_path,omitempty"`

	// ClientConfig is the consumer's own client descriptor, SANITIZED.
	// Carried so the dashboard can show what was actually deployed —
	// template, compute shape, schedules, the `uses:` list — without
	// the component guessing in advance which fields matter.
	ClientConfig map[string]any `json:"client_config,omitempty"`
}

// Process registers the consumer with the observatory service.
//
// Unlike the backup component's equivalent, this path mints no
// credentials and provisions nothing — it only reports facts about the
// deploy. That makes it safe to be non-fatal on failure (see below),
// which is a deliberate difference: a backup that silently does not
// happen is a data-loss risk, whereas a service missing from a
// dashboard is a visibility gap. Blocking a fleet deploy because the
// dashboard is briefly down would be the wrong trade.
func (p *Processor) Process(ctx *sdk.Context, _ api.Stack, input api.ResourceInput, collector pApi.ComputeContextCollector, params pApi.ProvisionParams) (*api.ResourceOutput, error) {
	resName := input.Descriptor.Name
	resType := input.Descriptor.Type
	stackName := input.StackParams.StackName

	serviceURL, err := p.readServiceURL(input, params)
	if err != nil {
		return nil, errors.Wrapf(err, "component %q unable to resolve service URL", resName)
	}
	signingKey, err := p.readSigningKey(input)
	if err != nil {
		return nil, errors.Wrapf(err, "component %q unable to resolve signingKey", resName)
	}

	// The signingKey doubles as the redaction-fingerprint key: it is the
	// one secret both sides of this component already share, and it
	// never leaves the operator's SC registry for the bucket the
	// fingerprints land in. See redact.
	rec := p.buildRecord(stackName, input, collector, p.cfg.ClientConfig, []byte(signingKey))

	token, err := mintJWT(signingKey, stackName, jwtLifetime)
	if err != nil {
		return nil, errors.Wrap(err, "mint registration JWT")
	}

	if err := p.pushRegister(ctx.Context(), serviceURL, token, rec); err != nil {
		// NON-FATAL, deliberately. A dashboard being unreachable must
		// not fail an unrelated service's deploy — observability is
		// additive. The warning lands in the deploy log, and the
		// service simply does not appear (or keeps its previous
		// record) until the next deploy.
		//
		// This is the opposite call from sc-component-backup, which
		// hard-fails: there, a skipped registration means a database
		// stops being backed up. Same mechanism, different stakes.
		if p.cfg.Progress != nil {
			p.cfg.Progress.Warn("compute-process", fmt.Sprintf(
				"component %q: registration to %s failed after %d attempts (%v) — the deploy continues; this service will be missing from the dashboard until its next deploy",
				resName, serviceURL, maxAttempts, err,
			))
		}
	} else if p.cfg.Progress != nil {
		p.cfg.Progress.Info("compute-process", fmt.Sprintf(
			"registered %q at %s (cloud=%s template=%s version=%s sources=%d inferred=%v)",
			stackName, serviceURL, rec.Cloud, rec.Template, rec.Version, len(rec.Sources), rec.Inferred,
		))
	}

	collector.AddEnvVariableIfNotExist(agentURLVar, serviceURL, resType, resName, stackName)

	return &api.ResourceOutput{
		Ref: sdk.String(fmt.Sprintf("observatory-compute:%s:%s", resType, resName)).ToStringOutput(),
	}, nil
}

// buildRecord assembles what this deploy can observe about itself.
//
// Version comes from SC's build pipeline, which injects VERSION on
// EVERY image build — so it is reliably present on a CI deploy. Commit
// is NOT auto-injected (the deploy action exports GITHUB_SHA and
// services opt into passing it as a build arg), so it is best-effort
// and the service falls back to the short SHA inside the CalVer string.
func (p *Processor) buildRecord(stackName string, input api.ResourceInput, collector pApi.ComputeContextCollector, clientCfg map[string]any, fpKey []byte) ServiceRecord {
	template := p.deriveTemplate(clientCfg)
	region := p.deriveRegion(input)

	rec := ServiceRecord{
		Stack:    stackName,
		Template: template,
		Version:  p.deriveVersion(input, collector),
		Commit:   p.deriveCommit(collector),
	}
	// Repository is only knowable from the bus — it appears in no
	// resource config and in no env a sibling contributes.
	rec.Repository = p.cfg.DeployIdentity.Repository
	if cloud, ok := CloudForTemplate(template); ok {
		rec.Cloud = cloud
	}

	// The consumer's own client config, from the api field added in
	// api#639. Sanitized here at the SOURCE rather than at render time,
	// because it lands in object storage and is served to a browser —
	// stripping downstream would mean every future endpoint has to
	// remember to do it.
	//
	// Absent against an older SC core, in which case the dashboard
	// simply has no config to show and the service's own health path
	// falls back to the component default.
	if len(clientCfg) > 0 {
		rec.ClientConfig = SanitizeClientConfig(clientCfg, fpKey)
		// The consumer's own public address. Without it the prober has
		// nothing to probe and the tenant sits at `unknown` forever,
		// which is how the first real tenant registered.
		//
		// Read from the UNSANITIZED config: a hostname is not a
		// credential, but `domain` would survive sanitization anyway —
		// taking it from the original keeps this independent of the
		// redaction rules, so tightening those later cannot silently
		// break health probing.
		rec.EndpointURL = EndpointURLFromClientConfig(clientCfg)
		// The consumer knows where its own probe lives better than a
		// fleet-wide default does.
		rec.HealthPath = HealthPathFromClientConfig(clientCfg)
		// A template that only names one region still deploys to one;
		// recording it lets the dashboard label the probe.
		if region := p.deriveRegion(input); region != "" {
			rec.Regions = []string{region}
		}
	}

	// Prefer handles the owning template plugin published; fall back to
	// inference and FLAG it. See PublishedSourcesKey / decision D1.
	if published := publishedSources(collector); len(published) > 0 {
		rec.Sources = published
		rec.Inferred = false
	} else if inferred := InferSources(stackName, template, region); len(inferred) > 0 {
		rec.Sources = inferred
		rec.Inferred = true
	}
	return rec
}

// publishedSources reads the template plugin's telemetry handles out of
// the compute context, if any plugin has learned to publish them.
func publishedSources(collector pApi.ComputeContextCollector) []TelemetrySource {
	raw := envFromCollector(collector, PublishedSourcesKey)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []TelemetrySource
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// envFromCollector returns the first non-empty value among names from
// the env the compute context has accumulated.
//
// Reads what PRECEDING `uses:` entries emitted, which is why
// `my-observatory` must be listed LAST — the same ordering constraint
// the backup component documents. SC core sorting Component processors
// last is an upstream follow-up.
func envFromCollector(collector pApi.ComputeContextCollector, names ...string) string {
	if collector == nil {
		return ""
	}
	envs := collector.EnvVariables()
	for _, name := range names {
		for _, e := range envs {
			if e.Name == name && strings.TrimSpace(e.Value) != "" {
				return e.Value
			}
		}
	}
	return ""
}

// deriveVersion resolves the deploy version, most authoritative first.
//
//  1. The bus header (api#640). This is the value the CI action passed
//     as `--deploy-version`, carried verbatim.
//  2. `VERSION` from a preceding `uses:` sibling, for a consumer that
//     routes it through the compute context itself.
//  3. `StackParams.Version`, which api#640 also fills from the header —
//     kept because an in-tree dispatch populates it directly.
//
// Returns "" when none resolve, and the SERVICE then falls back to what
// the deployed process reports on its own health endpoint. That is the
// honest ordering: a deploy-time version says what SC deployed, a
// probe-time one says what is running, and the service labels which it
// is showing rather than silently blending them.
func (p *Processor) deriveVersion(input api.ResourceInput, collector pApi.ComputeContextCollector) string {
	if v := p.cfg.DeployIdentity.Version; v != "" {
		return v
	}
	if v := envFromCollector(collector, "VERSION"); v != "" {
		return v
	}
	if input.StackParams != nil && input.StackParams.Version != "" {
		return input.StackParams.Version
	}
	return ""
}

// deriveCommit resolves the full commit SHA, bus first.
//
// The env fallback stays because it is what worked before api#640: a
// service that declares GITHUB_SHA under `image.build.args` routes it
// through the compute context. SC auto-injects only VERSION, so that
// opt-in is the only pre-#640 source.
func (p *Processor) deriveCommit(collector pApi.ComputeContextCollector) string {
	if c := p.cfg.DeployIdentity.Commit; c != "" {
		return c
	}
	return envFromCollector(collector, "GITHUB_SHA", "COMMIT", "REVISION")
}

// deriveTemplate names the template the CONSUMER deploys with.
//
// The consumer's, NOT this component's — and that distinction is the
// bug this function used to be. It read `deployTo` out of
// `configRaw(input)`, which is `my-observatory`'s OWN opaque config:
// `deployTo: lambda` says where the OBSERVATORY's Lambda runs and says
// nothing whatsoever about the service being registered. So every
// tenant registered as `template: lambda`.
//
// That was invisible for as long as every tenant happened to be a
// Lambda — all six were — and it became wrong the moment an
// `ecs-fargate` stack was wired in: the record claims `lambda`, the
// read-time derivation (registry.WithDerivedSources) therefore hands it
// `<stack>-access-logs` and `AWS/Lambda`, and both panels resolve to
// nothing. The failure mode is the component's own worst case — a
// confident answer about the wrong resource.
//
// The consumer's template rides the client descriptor (api#639),
// searched the nested way for the same reason the endpoint URL is: the
// descriptor is wrapped, `template` sits at its top level today, and
// hard-coding the path breaks in silence if api adds a level.
//
// Returns "" when there is no descriptor (an SC core older than #639).
// There is deliberately NO fallback to the component's own `deployTo`:
// guessing the consumer's template from a different stack's config is
// what produced the bug, and "" degrades honestly — CloudForTemplate
// declines, no handles are inferred, and the dashboard says it has no
// telemetry wired for the service rather than charting someone else's.
func (p *Processor) deriveTemplate(clientCfg map[string]any) string {
	return findStringDeep(clientCfg, "template")
}

func (p *Processor) deriveRegion(input api.ResourceInput) string {
	raw := configRaw(input)
	if st, ok := raw["storage"].(map[string]any); ok {
		if r, ok := st["region"].(string); ok && r != "" {
			return r
		}
	}
	return ""
}

// pushRegister POSTs the record with bounded retries. Each attempt gets
// its own timeout so total wall time stays bounded.
func (p *Processor) pushRegister(ctx context.Context, serviceURL, token string, rec ServiceRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return errors.Wrap(err, "marshal service record")
	}
	target := strings.TrimRight(serviceURL, "/") + registerPath

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := jitteredBackoff(backoffSeconds[attempt-2])
			select {
			case <-ctx.Done():
				return errors.Wrap(ctx.Err(), "retry loop cancelled")
			case <-time.After(delay):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		status, respBody, err := p.postOnce(attemptCtx, target, token, body)
		cancel()

		switch {
		case err != nil:
			lastErr = err
		case status >= 200 && status < 300:
			return nil
		case status == http.StatusForbidden || status == http.StatusUnauthorized:
			// Auth failures are not transient — retrying three times
			// just delays a deploy by 7 seconds to reach the same
			// answer. Surface immediately with the cause.
			return errors.Errorf("register rejected with HTTP %d (signingKey mismatch between plugin and service?): %s", status, respBody)
		default:
			lastErr = errors.Errorf("HTTP %d: %s", status, respBody)
		}
	}
	return lastErr
}

func (p *Processor) postOnce(ctx context.Context, target, token string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return resp.StatusCode, strings.TrimSpace(string(raw)), nil
}

// jitteredBackoff applies ±25% jitter so a fleet-wide deploy does not
// synchronise its retries into a thundering herd against a cold
// service.
func jitteredBackoff(seconds int) time.Duration {
	base := float64(seconds) * float64(time.Second)
	jitter := (rand.Float64() - 0.5) / 2 // −0.25 … +0.25
	return time.Duration(base * (1 + jitter))
}

// mintJWT produces the short-TTL registration token. `op: register`
// and a `tenant` claim pinned to the consumer stack, so the service can
// refuse a consumer registering under another's name.
func mintJWT(signingKey, stackName string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"op":     "register",
		"tenant": stackName,
		"iss":    "sc-component-observatory",
		"sub":    stackName,
		"iat":    now.Unix(),
		"nbf":    now.Add(-5 * time.Second).Unix(),
		"exp":    now.Add(ttl).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(signingKey))
}
