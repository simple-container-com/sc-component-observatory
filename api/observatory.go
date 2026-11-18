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
// Design goal, inherited as a lesson from the backup roll: anything the
// plugin can derive, the plugin derives. Every field here is either
// genuinely operator-specific (a hostname, a secret alias) or has a
// working default. Compute sizing, ECS cluster/task-family names,
// subnets, security groups, public-IP assignment, log groups, the
// service's own callback URL and the telemetry-reader role are all
// plugin-side and must never appear in server.yaml.
type ObservatoryConfig struct {
	// DeployTo names a top-level template from the parent server.yaml's
	// `templates:` block. Required; validated at parse time.
	//
	// `lambda` is the expected value and the only one exercised in
	// slice 1 — it matches sc-component-backup and has the cheapest
	// idle cost. `ecs-fargate` is the fallback if long-running log
	// queries ever outgrow the dispatch-to-worker shape.
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
	// registration JWTs at ComputeProcess time) and the deployed
	// service (which verifies them). Required.
	SigningKey string `json:"signingKey" yaml:"signingKey"`

	// OperatorPassword is a `${secret:...}` reference to the shared
	// password the SPA's /login handler validates. Required for the
	// operator UI; the service refuses POST /api/v1/login when unset.
	OperatorPassword string `json:"operatorPassword,omitempty" yaml:"operatorPassword,omitempty"`

	// Ingress declares the public entry point. The hostname is also how
	// the service learns its OWN URL: the plugin derives the registration
	// endpoint and the worker callback URL from it at PreProvision time,
	// so neither is operator-written.
	Ingress *Ingress `json:"ingress,omitempty" yaml:"ingress,omitempty"`

	// Storage declares where observatory keeps its own state — the
	// service registry, deploy history, saved queries and cached
	// long-query results. NOT telemetry: logs and metrics are read from
	// the target cloud's own stores and never ingested here.
	//
	// Optional. Defaults to `type: s3` with a plugin-provisioned bucket.
	Storage *Storage `json:"storage,omitempty" yaml:"storage,omitempty"`

	// Metadata selects the backing store for structured records.
	// Optional; defaults to `colocated` — JSON objects under a `_meta/`
	// prefix in the same bucket, so an operator who names only a bucket
	// gets a complete deployment with no second store to provision.
	Metadata *Metadata `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Readers holds read-only credentials for clouds observatory is NOT
	// deployed into, keyed by provider name ("yandex", "kubernetes").
	//
	// Same-cloud needs nothing: the plugin attaches a telemetry-reader
	// role to observatory's own identity at Provision time. Cross-cloud
	// is an honest floor — a Lambda in AWS cannot read Yandex Logging
	// without a Yandex credential, and silently rendering empty charts
	// would be worse than asking for one secret.
	//
	// Omit entirely for a single-cloud fleet.
	Readers map[string]string `json:"readers,omitempty" yaml:"readers,omitempty"`

	// Env is optional plain env injected into the deployed container,
	// merged over the template's own (plugin values win on collision).
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
}

// Ingress is the public entry point for the deployed service.
type Ingress struct {
	// Hostname is the DNS name the service answers on, e.g.
	// "observatory.simple-forge.com". SC core strips this before
	// dispatching the compute template and wires DNS itself from the
	// endpoint URL the template publishes — see
	// forge/docs/design/2026-11-17/component-parent-dns-wiring/.
	Hostname string `json:"hostname" yaml:"hostname"`
}

// Storage is observatory's own object-storage target.
//
// Mirrors sc-component-backup's pluggable storage so an operator who
// already runs one Component knows this shape. Backends: s3 (including
// any S3-compatible gateway via EndpointURL), gcs, filesystem (a PVC,
// an EFS mount, or a plain VM path).
type Storage struct {
	// Type is one of "s3", "filesystem". Defaults to "s3".
	//
	// "gcs" is declared in the token list below as the forward-looking
	// contract but is NOT accepted yet: the GCS backend would pull the
	// whole google.golang.org/api tree into every build for a path the
	// AWS-first slices never exercise. Validation rejects it with a
	// clear message rather than letting an operator write config that
	// only fails at container boot.
	Type string `json:"type,omitempty" yaml:"type,omitempty"`

	// Bucket is the S3/GCS bucket. When empty on an s3/gcs deploy the
	// plugin provisions one named after the component + environment, so
	// the operator does not have to pre-create it.
	Bucket string `json:"bucket,omitempty" yaml:"bucket,omitempty"`

	// Region defaults to the parent stack's region.
	Region string `json:"region,omitempty" yaml:"region,omitempty"`

	// Prefix optionally namespaces every key, for a shared bucket.
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`

	// EndpointURL targets an S3-compatible gateway (MinIO, SeaweedFS,
	// Ceph). Empty means AWS S3.
	EndpointURL string `json:"endpointUrl,omitempty" yaml:"endpointUrl,omitempty"`

	// UsePathStyle forces path-style addressing. Required by most
	// S3-compatible gateways; harmless on AWS.
	UsePathStyle bool `json:"usePathStyle,omitempty" yaml:"usePathStyle,omitempty"`

	// Path is the mount point when Type is "filesystem" — a k8s PVC, an
	// ECS/EFS mount, or a VM directory.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// AccessKey / SecretKey are `${secret:...}` refs. Omit on AWS to use
	// the execution role, which is the fleet-preferred shape.
	AccessKey string `json:"accessKey,omitempty" yaml:"accessKey,omitempty"`
	SecretKey string `json:"secretKey,omitempty" yaml:"secretKey,omitempty"`
}

// Metadata selects the structured-record backing store.
type Metadata struct {
	// Type is one of "colocated", "mongodb", "sqlite", "postgres",
	// "mysql". Defaults to "colocated".
	Type string `json:"type,omitempty" yaml:"type,omitempty"`

	// URI is the connection string for the database types. A
	// `${secret:...}` ref. Ignored for "colocated".
	URI string `json:"uri,omitempty" yaml:"uri,omitempty"`

	// Path is the sqlite file location. Ignored for other types.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// Storage backend + metadata type tokens.
const (
	StorageTypeS3         = "s3"
	StorageTypeGCS        = "gcs"
	StorageTypeFilesystem = "filesystem"

	MetadataTypeColocated = "colocated"
	MetadataTypeMongoDB   = "mongodb"
	MetadataTypeSQLite    = "sqlite"
	MetadataTypePostgres  = "postgres"
	MetadataTypeMySQL     = "mysql"
)

// DefaultStorage / DefaultMetadata are what an omitted block resolves
// to. Exported so the plugin and the service agree on the default
// rather than each hardcoding it.
func DefaultStorage() Storage   { return Storage{Type: StorageTypeS3} }
func DefaultMetadata() Metadata { return Metadata{Type: MetadataTypeColocated} }

// Validate checks the config at parse time.
//
// Errors must name the offending key and say what to do about it: a
// Component config error surfaces during `sc provision` long before any
// resource exists, and a vague message there costs an 8-minute
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
	return c.validateMetadata()
}

func (c *ObservatoryConfig) validateStorage() error {
	if c.Storage == nil {
		return nil
	}
	switch t := c.Storage.Type; t {
	case "", StorageTypeS3:
		// Bucket may be empty — the plugin provisions one.
	case StorageTypeGCS:
		return errors.Errorf("`storage.type: gcs` is not implemented in this build of %q yet — use %q (which also serves any S3-compatible gateway via `storage.endpointUrl`) or %q", ResourceType, StorageTypeS3, StorageTypeFilesystem)
	case StorageTypeFilesystem:
		if c.Storage.Path == "" {
			return errors.Errorf("`storage.path` is required when `storage.type: filesystem` for %q — point it at a PVC, an EFS mount, or a VM directory", ResourceType)
		}
		// Lambda's /tmp is ephemeral and per-container, so a filesystem
		// store there silently loses the registry on every cold start.
		if c.DeployTo == "lambda" {
			return errors.Errorf("`storage.type: filesystem` is incompatible with `deployTo: lambda` for %q — Lambda's /tmp is ephemeral and not shared between containers, so the registry would be lost on cold start; use `storage.type: s3`", ResourceType)
		}
	default:
		return errors.Errorf("unknown `storage.type` %q for %q — expected %q or %q", t, ResourceType, StorageTypeS3, StorageTypeFilesystem)
	}
	return nil
}

func (c *ObservatoryConfig) validateMetadata() error {
	if c.Metadata == nil {
		return nil
	}
	switch t := c.Metadata.Type; t {
	case "", MetadataTypeColocated:
		// Colocated rides the storage backend; nothing more required.
		// Reject it explicitly against a filesystem-less shape rather
		// than failing at runtime with a confusing storage error.
		if c.Storage != nil && c.Storage.Type == StorageTypeFilesystem && c.Storage.Path == "" {
			return errors.Errorf("`metadata.type: colocated` needs a usable `storage` block for %q", ResourceType)
		}
	case MetadataTypeSQLite:
		if c.Metadata.Path == "" {
			return errors.Errorf("`metadata.path` is required when `metadata.type: sqlite` for %q", ResourceType)
		}
		if c.DeployTo == "lambda" {
			return errors.Errorf("`metadata.type: sqlite` is incompatible with `deployTo: lambda` for %q — the file would live on ephemeral per-container /tmp; use `colocated` (the default) or a managed database", ResourceType)
		}
	case MetadataTypeMongoDB, MetadataTypePostgres, MetadataTypeMySQL:
		if c.Metadata.URI == "" {
			return errors.Errorf("`metadata.uri` is required when `metadata.type: %s` for %q — a `${secret:...}` ref to the connection string", t, ResourceType)
		}
	default:
		return errors.Errorf("unknown `metadata.type` %q for %q — expected one of %q, %q, %q, %q, %q", t, ResourceType,
			MetadataTypeColocated, MetadataTypeMongoDB, MetadataTypeSQLite, MetadataTypePostgres, MetadataTypeMySQL)
	}
	return nil
}

// ResolvedStorage returns the storage block with defaults applied.
func (c *ObservatoryConfig) ResolvedStorage() Storage {
	if c.Storage == nil {
		return DefaultStorage()
	}
	s := *c.Storage
	if s.Type == "" {
		s.Type = StorageTypeS3
	}
	return s
}

// ResolvedMetadata returns the metadata block with defaults applied.
func (c *ObservatoryConfig) ResolvedMetadata() Metadata {
	if c.Metadata == nil {
		return DefaultMetadata()
	}
	m := *c.Metadata
	if m.Type == "" {
		m.Type = MetadataTypeColocated
	}
	return m
}
