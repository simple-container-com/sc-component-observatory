// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Package api declares the operator-facing config for the
// `sc-observatory` Component.
//
// Doc-only, same as sc-component-backup's /api module: the production
// `sc` binary does not link plugin /api modules, so a plugin-owned
// config type cannot be Go-interface-asserted against — dispatch reads
// `.Raw` instead (sc-components-platform design doc §P1 v1.5). These
// types exist so the shape is reviewable, documented in one place, and
// usable by the plugin's own validation.
package api

import "github.com/pkg/errors"

// ResourceType is the token an operator writes under
// `resources.<env>.resources.<name>.type`.
const ResourceType = "sc-observatory"

// ObservatoryConfig is the `config:` block of an `sc-observatory`
// resource.
//
// The governing rule: NO cloud-specific detail belongs here. No bucket
// names, no regions, no IAM policies, no subnets, no log groups, no
// cluster names. Those are all either derived by the plugin from the
// chosen template or read from another SC resource the operator already
// declared. What is left is genuinely the operator's decision: which
// template to deploy to, which image, which hostname, which SC resource
// to keep state in, and two secret aliases.
//
// Anything a plugin can derive, the plugin derives — the lesson that
// cost sc-component-backup two deploy round-trips when `maxMemory`,
// ECS cluster names and subnet lists were all operator-written before
// being moved plugin-side.
type ObservatoryConfig struct {
	// DeployTo names a top-level template from the parent server.yaml's
	// `templates:` block — `lambda`, `ecs-fargate`, `yc-container`,
	// `kubernetes`. Required.
	//
	// This single field is what makes the component portable: the
	// plugin reads it to decide which telemetry permissions to request
	// and which cloud's APIs the deployed service will query. The
	// operator never writes those consequences out.
	DeployTo string `json:"deployTo" yaml:"deployTo"`

	// Image is the registry reference for the observatory service
	// container. Required.
	//
	// Pin a concrete tag, never `:latest`: SC's ECR rewrap keys its
	// build context on the image reference, so `:latest` leaves Pulumi
	// caching the previous digest and the deployed service silently
	// keeps running the old code (live-caught on sc-component-backup).
	Image string `json:"image" yaml:"image"`

	// SigningKey is a `${secret:...}` reference to a 32+-byte HS256 key
	// shared between the plugin (which mints short-TTL per-consumer
	// registration JWTs) and the deployed service (which verifies
	// them). Required.
	SigningKey string `json:"signingKey" yaml:"signingKey"`

	// OperatorPassword is a `${secret:...}` reference to the shared
	// password the SPA's login validates. Required for the operator UI.
	OperatorPassword string `json:"operatorPassword,omitempty" yaml:"operatorPassword,omitempty"`

	// MigrationToken is a `${secret:...}` reference to the bearer the
	// one-off `POST /admin/migrate-s3-to-goembedb` endpoint demands. Set
	// it for the duration of the S3 → go-embedb cutover and REMOVE it
	// afterwards: absent, the service does not mount the route at all,
	// which is the steady state.
	//
	// Not a plain env var by design. Like SigningKey and
	// OperatorPassword it is bound through ComponentSecretEnvMap, so the
	// value lands in a per-function SecretsManager entry and the service
	// resolves the ARN at startup — the token that can rewrite the whole
	// datastore should not be readable from the Lambda's environment.
	MigrationToken string `json:"migrationToken,omitempty" yaml:"migrationToken,omitempty"`

	// Ingress declares the public entry point. The hostname is also how
	// the service learns its OWN url — the plugin derives it at
	// PreProvision, so it is never written twice.
	Ingress *Ingress `json:"ingress,omitempty" yaml:"ingress,omitempty"`

	// Storage names ANOTHER SC RESOURCE to keep observatory's own state
	// in — the service registry, deploy history and healthcheck
	// results. Not telemetry: logs and metrics are read from the target
	// cloud at query time and never ingested.
	//
	// This is a reference, not a configuration. The operator declares
	// (say) an `s3-bucket` resource in the same server.yaml and points
	// at it by name; the plugin reads that resource's own outputs for
	// the bucket and region. Swapping S3 for an EFS volume, a PVC, or a
	// future document store is then a change of which resource is
	// named — not a change to observatory's config, and not a change to
	// observatory's code.
	//
	// Optional. Omitted means the plugin provisions its own bucket for
	// the component, so a minimal deployment needs no storage block at
	// all.
	Storage *StorageRef `json:"storage,omitempty" yaml:"storage,omitempty"`

	// Healthcheck tunes the probe loop. Entirely optional — the
	// defaults are sensible and cloud-agnostic.
	Healthcheck *Healthcheck `json:"healthcheck,omitempty" yaml:"healthcheck,omitempty"`

	// Readers holds read-only credentials for clouds observatory is NOT
	// deployed into, keyed by provider name ("yandex", "kubernetes").
	//
	// Same-cloud needs nothing: the plugin requests telemetry-read
	// permissions for observatory's own identity as part of the deploy.
	// Cross-cloud is an honest floor — a Lambda in AWS cannot read
	// Yandex Logging without a Yandex credential, and silently
	// rendering empty charts would be worse than asking for one secret.
	//
	// Omit entirely for a single-cloud fleet.
	Readers map[string]string `json:"readers,omitempty" yaml:"readers,omitempty"`

	// Env is optional plain env injected into the deployed container.
	// An escape hatch, not a configuration surface — if something ends
	// up here routinely, it belongs plugin-side instead.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

// Ingress is the public entry point for the deployed service.
type Ingress struct {
	// Hostname is the DNS name the service answers on, e.g.
	// "monitor.simple-forge.com". SC core strips this before
	// dispatching the compute template and wires DNS itself from the
	// endpoint URL the template publishes — see
	// forge/docs/design/2026-11-17/component-parent-dns-wiring/.
	Hostname string `json:"hostname" yaml:"hostname"`
}

// StorageRef points at another SC resource by name and type.
//
// YAML accepts either form:
//
//	storage: observatory-data          # type inferred as s3-bucket
//
//	storage:
//	  name: observatory-data
//	  type: s3-bucket
//
// The bare-string form exists because naming the resource is the whole
// decision in the common case, and a one-line config beats a two-line
// one. UnmarshalYAML/JSON on the plugin side accepts both.
type StorageRef struct {
	// Name is the SC resource name, as declared elsewhere in the same
	// server.yaml `resources:` block. Required unless the whole block
	// is omitted.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`

	// Type is the referenced resource's SC type. Defaults to
	// "s3-bucket". Declared explicitly so the plugin knows which of
	// that resource's outputs to read — an s3-bucket exports a bucket
	// name and region, a future volume resource would export a mount
	// path — without sniffing or guessing.
	Type string `json:"type,omitempty" yaml:"type,omitempty"`

	// Prefix optionally namespaces observatory's keys inside the
	// referenced resource, so one bucket can host several components.
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`
}

// Referenced storage resource types the plugin knows how to read.
const (
	// StorageResourceS3Bucket is an SC `s3-bucket` resource. The plugin
	// reads its `<name>-bucket-name` and `<name>-bucket-region`
	// exports.
	StorageResourceS3Bucket = "s3-bucket"

	// StorageResourceVolume is a filesystem-backed resource — an EFS
	// mount, a Kubernetes PVC, a VM directory. Declared now because the
	// operator asked for it as a first-class alternative; the plugin
	// rejects it with a clear message until the backing resource types
	// are wired, rather than accepting config that fails at boot.
	StorageResourceVolume = "volume"

	// StorageResourceMongoDB is a document store — the eventual home
	// for richer history than JSON objects comfortably hold. Same
	// "declared, not yet implemented" status as volume.
	StorageResourceMongoDB = "mongodb"
)

// Healthcheck tunes the probe loop that runs against every registered
// service.
//
// Cloud-agnostic by construction: the probe is an HTTP GET against the
// endpoint each service reported at registration. That works for a
// Lambda Function URL, a YC container URL, an ingress hostname and a
// plain VM alike, which is why it is the default rather than a
// per-cloud health API.
type Healthcheck struct {
	// Enabled defaults to true. Set false to register services and
	// collect deploy history without probing them.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`

	// Path is appended to each service's endpoint URL. Defaults to
	// "/healthz", the fleet convention.
	//
	// A service that reports its own health path at registration (from
	// its client config — a compose healthcheck, say) overrides this
	// per-service, so this is only the fallback.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// IntervalMinutes is how often the probe loop runs. Defaults to 5.
	// The plugin translates this into whatever scheduling primitive the
	// chosen template offers (an EventBridge rule on Lambda, a cron
	// sidecar elsewhere) — the operator does not write a cron
	// expression.
	IntervalMinutes int `json:"intervalMinutes,omitempty" yaml:"intervalMinutes,omitempty"`

	// TimeoutSeconds per probe. Defaults to 5 — long enough for a cold
	// Lambda to answer, short enough that one hung service does not
	// stall the sweep.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`

	// RetainDays caps how long probe results are kept. Defaults to 30.
	// Results are small, but they accumulate per service per interval
	// forever otherwise.
	RetainDays int `json:"retainDays,omitempty" yaml:"retainDays,omitempty"`
}

// Healthcheck defaults, exported so the plugin and service agree
// instead of each hardcoding them.
const (
	DefaultHealthcheckPath     = "/healthz"
	DefaultHealthcheckInterval = 5
	DefaultHealthcheckTimeout  = 5
	DefaultHealthcheckRetain   = 30
)

// Resolved returns the healthcheck config with defaults applied.
func (h *Healthcheck) Resolved() Healthcheck {
	out := Healthcheck{
		Path:            DefaultHealthcheckPath,
		IntervalMinutes: DefaultHealthcheckInterval,
		TimeoutSeconds:  DefaultHealthcheckTimeout,
		RetainDays:      DefaultHealthcheckRetain,
	}
	enabled := true
	out.Enabled = &enabled
	if h == nil {
		return out
	}
	if h.Enabled != nil {
		out.Enabled = h.Enabled
	}
	if h.Path != "" {
		out.Path = h.Path
	}
	if h.IntervalMinutes > 0 {
		out.IntervalMinutes = h.IntervalMinutes
	}
	if h.TimeoutSeconds > 0 {
		out.TimeoutSeconds = h.TimeoutSeconds
	}
	if h.RetainDays > 0 {
		out.RetainDays = h.RetainDays
	}
	return out
}

// IsEnabled reports whether probing should run.
func (h *Healthcheck) IsEnabled() bool {
	r := h.Resolved()
	return r.Enabled == nil || *r.Enabled
}

// ResolvedStorage returns the storage reference with defaults applied.
// A nil block means "plugin provisions its own", signalled by an empty
// Name.
func (c *ObservatoryConfig) ResolvedStorage() StorageRef {
	if c.Storage == nil {
		return StorageRef{Type: StorageResourceS3Bucket}
	}
	s := *c.Storage
	if s.Type == "" {
		s.Type = StorageResourceS3Bucket
	}
	return s
}

// Validate checks the config at parse time.
//
// Errors must name the offending key AND say what to do about it: a
// Component config error surfaces during `sc provision`, long before
// any resource exists, and a vague message there costs a slow
// round-trip to diagnose.
func (c *ObservatoryConfig) Validate() error {
	if c.DeployTo == "" {
		return errors.Errorf("`deployTo` is required for %q — name a template from the parent stack's `templates:` block (e.g. `lambda`)", ResourceType)
	}
	if c.Image == "" {
		return errors.Errorf("`image` is required for %q — pin a concrete tag (never `:latest`, which leaves Pulumi caching the previous digest)", ResourceType)
	}
	if c.SigningKey == "" {
		return errors.Errorf("`signingKey` is required for %q — a `${secret:...}` ref to a 32+-byte HS256 key", ResourceType)
	}
	if c.Ingress != nil && c.Ingress.Hostname == "" {
		return errors.Errorf("`ingress.hostname` is empty for %q — remove the `ingress:` block entirely or give it a hostname", ResourceType)
	}
	if err := c.validateStorage(); err != nil {
		return err
	}
	return c.validateHealthcheck()
}

func (c *ObservatoryConfig) validateStorage() error {
	if c.Storage == nil {
		return nil
	}
	s := c.ResolvedStorage()
	switch s.Type {
	case StorageResourceS3Bucket:
		if s.Name == "" {
			return errors.Errorf("`storage.name` is required for %q when a `storage:` block is present — name an `s3-bucket` resource declared elsewhere in this server.yaml, or omit the block entirely to have one provisioned", ResourceType)
		}
	case StorageResourceVolume, StorageResourceMongoDB:
		return errors.Errorf("`storage.type: %s` is declared but not implemented yet for %q — use %q (the default) for now; see the component's README for the slice that adds it", s.Type, ResourceType, StorageResourceS3Bucket)
	default:
		return errors.Errorf("unknown `storage.type` %q for %q — expected %q (or the not-yet-implemented %q / %q)", s.Type, ResourceType, StorageResourceS3Bucket, StorageResourceVolume, StorageResourceMongoDB)
	}
	return nil
}

func (c *ObservatoryConfig) validateHealthcheck() error {
	if c.Healthcheck == nil {
		return nil
	}
	h := c.Healthcheck
	if h.IntervalMinutes < 0 {
		return errors.Errorf("`healthcheck.intervalMinutes` must be positive for %q", ResourceType)
	}
	if h.TimeoutSeconds < 0 {
		return errors.Errorf("`healthcheck.timeoutSeconds` must be positive for %q", ResourceType)
	}
	if h.RetainDays < 0 {
		return errors.Errorf("`healthcheck.retainDays` must be positive for %q", ResourceType)
	}
	if h.Path != "" && h.Path[0] != '/' {
		return errors.Errorf("`healthcheck.path` must start with '/' for %q, got %q", ResourceType, h.Path)
	}
	return nil
}
