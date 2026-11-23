// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package observatory

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pkg/errors"

	"github.com/simple-container-com/api/pkg/api"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
)

// configRaw reads the component's opaque config map off the resource
// descriptor, returning an empty map rather than nil so callers can
// index without guarding.
//
// SC core carries Component config as an untyped `.Raw` map because the
// production `sc` binary does not link plugin /api modules — the typed
// ObservatoryConfig in this repo's api/ package is documentation and
// plugin-side validation only (platform doc §P1 v1.5).
func configRaw(input api.ResourceInput) map[string]any {
	if raw, ok := extractConfigRaw(input.Descriptor); ok && raw != nil {
		return raw
	}
	// The ComponentRaw side-channel is the other carrier; on the
	// compute-process path SC core packs the component's map there.
	if input.ComponentRaw != nil {
		return input.ComponentRaw
	}
	return map[string]any{}
}

func extractConfigRaw(desc *api.ResourceDescriptor) (map[string]any, bool) {
	if desc == nil || desc.Config.Config == nil {
		return nil, false
	}
	type rawHaver interface{ GetRaw() map[string]any }
	if rh, ok := desc.Config.Config.(rawHaver); ok {
		return rh.GetRaw(), true
	}
	// Tests and local dev hand a plain map.
	if m, ok := desc.Config.Config.(map[string]any); ok {
		return m, true
	}
	return nil, false
}

// readSigningKey pulls the HS256 key the service validates registration
// tokens with.
//
// Must be the same value the service ends up holding. On AWS the
// service receives it as a SecretsManager ARN and resolves it at
// startup; the plugin receives the resolved secret because SC
// substitutes `${secret:...}` before dispatch. A mismatch here is the
// single most likely cause of a 401 on every registration, which is why
// pushRegister treats 401/403 as non-transient and says so.
func (p *Processor) readSigningKey(input api.ResourceInput) (string, error) {
	if v, ok := configRaw(input)["signingKey"].(string); ok && v != "" {
		return v, nil
	}
	return "", errors.New("signingKey not found on the component config — operator must declare `config.signingKey: ${secret:...}`")
}

// readServiceURL resolves the deployed observatory service's base URL.
//
// Cascade, in order of trustworthiness:
//
//  1. `_deployed_service` / `_endpoint_url` from the dependency outputs
//     SC core's Path A sidecar publishes after dispatching the compute
//     template. This is the real, live URL.
//  2. `ingress.hostname` off the component config. Correct once DNS has
//     propagated, and the only option available when SC core has not
//     populated dependency outputs — which it does not do on the
//     consumer-side ComputeProcess path today
//     (api/pkg/clouds/pulumi/provision.go:373). The backup component
//     hit exactly this and had to add a fallback.
//
// Returns an error naming both options when neither is present, rather
// than defaulting to something that will silently fail to connect.
func (p *Processor) readServiceURL(input api.ResourceInput, params pApi.ProvisionParams) (string, error) {
	// Literals, not the const: the canonical ComponentEndpointURLKey
	// lives in SC core's engine package (pkg/clouds/pulumi), which a
	// plugin must not import — plugins talk to core over the bus, not
	// by linking it. If core ever renames these, the dependency-output
	// lookup silently stops matching and the ingress.hostname fallback
	// below takes over, which is a degraded-but-working outcome rather
	// than a failure.
	for _, key := range []string{"_endpoint_url", "_deployed_service"} {
		if u := lookupDepString(params, key); u != "" {
			return normalizeURL(u), nil
		}
	}
	if ing, ok := configRaw(input)["ingress"].(map[string]any); ok {
		if host, ok := ing["hostname"].(string); ok && host != "" {
			return normalizeURL(host), nil
		}
	}
	return "", errors.New(
		"cannot resolve the observatory service URL: no `_endpoint_url`/`_deployed_service` in the dependency outputs and no `config.ingress.hostname` on the component",
	)
}

// lookupDepString searches the sibling-resource outputs for a string
// value under `key`, across every resource that published one.
//
// The published values are NOT on the `*api.ResourceOutput` itself —
// that type is `struct{ Ref any }` and nothing else. A plugin-owned
// resource leaves an `*api.PluginResourceOutput` on `.Ref` and the
// values live in its `Outputs map[string]PluginOutputValue`
// (api/pkg/api/plugin_resource_output.go). Until 2026-11-23 this
// function type-asserted the *ResourceOutput POINTER against
// `map[string]any`, which a struct pointer can never satisfy — so the
// first and most trustworthy branch of readServiceURL's cascade was
// unreachable and every resolution fell through to `ingress.hostname`.
// Nothing went red, because no test built a dependency-output fixture
// the way the plugin bus builds one (see TestDependencyEndpointURL...).
// Found while writing the same test for sc-component-notifier, which
// inherited this file.
//
// `map[string]any` is still accepted on `.Ref` because an in-tree
// (non-plugin) resource may put its own typed output there.
//
// Resource names are visited in sorted order: Go map iteration is
// randomised, so when two dependencies publish the same key an
// unsorted walk picks a different one per run, with nothing in the diff
// to explain it.
func lookupDepString(params pApi.ProvisionParams, key string) string {
	names := make([]string, 0, len(params.ResourceOutputs))
	for name := range params.ResourceOutputs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		out := params.ResourceOutputs[name]
		if out == nil || out.Ref == nil {
			continue
		}
		switch ref := out.Ref.(type) {
		case *api.PluginResourceOutput:
			if v, found := ref.Lookup(key); found {
				if s, ok := v.Value.(string); ok && s != "" {
					return s
				}
			}
		case map[string]any:
			if v, ok := ref[key].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// normalizeURL accepts a bare hostname or a full URL and returns a
// scheme-qualified base with no trailing slash.
//
// Bare hostnames get https:// — an SC Component always sits behind TLS
// (a Lambda Function URL or a registrar-managed record), and defaulting
// to http would produce a redirect-or-fail on every registration.
//
// Whitespace comes off FIRST. Trimming slashes from a value that still
// has a trailing space is a no-op, so `"https://host/ "` kept its
// trailing slash and every registration POST went to `//api/v1/...`.
// Shipped in that order until 2026-11-23.
func normalizeURL(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://" + s
}

// SelfURLFromConfig derives the component's own public URL from its
// `ingress.hostname`, for the PreProvision tpl-extension that tells the
// deployed service where it lives.
//
// Empty when no ingress is declared, which is legitimate: a component
// with no public hostname can still serve a registry reachable from
// inside the VPC, and inventing a URL would be worse than omitting one.
func SelfURLFromConfig(raw map[string]any) string {
	ing, ok := raw["ingress"].(map[string]any)
	if !ok {
		return ""
	}
	host, ok := ing["hostname"].(string)
	if !ok || host == "" {
		return ""
	}
	return normalizeURL(host)
}

// DefaultBucketName is the storage bucket the plugin provisions when
// the operator names none, keyed to the component + environment so two
// components in one account cannot collide.
func DefaultBucketName(resourceName, env string) string {
	name := fmt.Sprintf("sc-observatory-%s-%s", resourceName, env)
	// S3 bucket names: lowercase, 3–63 chars, no underscores.
	name = strings.ToLower(strings.ReplaceAll(name, "_", "-"))
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.Trim(name, "-")
}

// BucketForStorageRef resolves the operator's `storage:` declaration to
// the physical bucket the deployed service should read and write.
//
// The operator's side of the contract is one word — the NAME of another
// SC resource declared in the same server.yaml:
//
//	sc-observatory-data:
//	  type: s3-bucket
//	  config: { credentials: "${auth:aws}", allowOnlyHttps: true }
//
//	my-observatory:
//	  type: sc-observatory
//	  config: { storage: sc-observatory-data, ... }
//
// No bucket name, no region, no credential. That indirection is the
// point: repointing storage at a different resource — a volume, a
// document store — is a one-word change with no component code
// involved, which is what keeps the component generic across deploy
// templates.
//
// Why a CONVENTION and not a lookup: the referenced resource is
// provisioned in the same parent-stack update as this component, so at
// PreProvision time it does not exist yet and its outputs cannot be
// read. The sc-plugin-aws s3-bucket resource names its bucket
// `<resourceName>--<env>` when the operator sets no explicit
// `bucketName`, so that is derivable without a lookup. Verified against
// a real preview plan rather than from reading the other plugin:
// `storage: sc-observatory-data` in env `staging` planned
// `aws:s3/bucket:Bucket::sc-observatory-data--staging` with
// `bucket: "sc-observatory-data--staging"` (forge run 44469566225).
//
// The hole that leaves is an s3-bucket resource carrying an explicit
// `bucketName`, which the convention cannot see. That case sets
// `storage.bucket` explicitly — hence the map form below, which also
// covers a bucket provisioned outside SC entirely.
//
// Accepted shapes, in order:
//
//  1. `storage: {bucket: "literal-name"}` — explicit, wins over
//     everything. Also the escape hatch above.
//  2. `storage: {name: "resource-name"}` — the reference, map form.
//  3. `storage: "resource-name"` — the reference, the shape an
//     operator actually writes.
//  4. absent — the component names its own bucket. Keeps a
//     zero-config deploy working, and is why `storage:` is optional.
func BucketForStorageRef(raw map[string]any, resourceName, env string) string {
	switch st := raw["storage"].(type) {
	case string:
		if name := strings.TrimSpace(st); name != "" {
			return scResourceBucketName(name, env)
		}
	case map[string]any:
		if b, ok := st["bucket"].(string); ok && strings.TrimSpace(b) != "" {
			return strings.TrimSpace(b)
		}
		if n, ok := st["name"].(string); ok && strings.TrimSpace(n) != "" {
			return scResourceBucketName(strings.TrimSpace(n), env)
		}
	}
	return DefaultBucketName(resourceName, env)
}

// scResourceBucketName mirrors the s3-bucket resource's own naming:
// `<resourceName>--<env>`.
//
// The double hyphen is SC's resource-name/environment separator and is
// not cosmetic — a single one would collide with a resource name that
// itself ends in `-staging`.
func scResourceBucketName(resourceName, env string) string {
	if env == "" {
		return resourceName
	}
	return resourceName + "--" + env
}
