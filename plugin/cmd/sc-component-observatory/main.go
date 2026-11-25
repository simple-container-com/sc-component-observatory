// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Command sc-component-observatory is the SC plugin half of the
// sc-observatory Component.
//
// It implements four RPCs that matter:
//
//	Schema          — declares the resource type, the operator-config
//	                  allowlist, and plugin-side defaults.
//	PreProvision    — lands derived values (self-URL, storage bucket)
//	                  as tpl extensions BEFORE the compute template is
//	                  dispatched, so the deployed service receives them
//	                  as env without the operator writing any.
//	ComputeProcess  — fires per consumer that lists this component in
//	                  `uses:`; push-registers the deploy.
//	PreDestroy      — best-effort cleanup when the component itself is
//	                  torn down.
//
// Provision is a near-no-op: SC core's Path A sidecar dispatches the
// operator-chosen compute template (aws-lambda, etc.) BEFORE this
// plugin's Provision runs, so the service already exists by then.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/hashicorp/go-hclog"
	hplugin "github.com/hashicorp/go-plugin"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
	"github.com/simple-container-com/api/pkg/plugin/runtime"

	observatoryApi "github.com/simple-container-com/sc-component-observatory/api"
	observatory "github.com/simple-container-com/sc-component-observatory/plugin/internal"
)

// version is stamped by the release workflow via -ldflags.
var version = "dev"

type observatoryPlugin struct {
	scpluginv1.UnimplementedSCCloudPluginServer
	progress *scPlugin.Publisher
	broker   *hplugin.GRPCBroker
}

// SetBroker implements scPlugin.BrokerAware.
func (p *observatoryPlugin) SetBroker(b *hplugin.GRPCBroker) { p.broker = b }

// componentDefaultsJSON is merged UNDER the operator's config at
// dispatch time — operator wins per key.
//
// This is where the "config under ten lines" goal is actually
// delivered. Every entry here is something the first SC Component made
// an operator write by hand before the lesson landed:
//
//   - maxMemory/timeout: sized from measurement, not the SC default.
//     The backup component shipped at the 128 MB default, OOMed, got
//     band-aided to 2048, and was finally measured at ~100 MB steady
//     state and set to 512. Starting from a measured number skips that
//     whole loop. Observatory's workload is smaller still (no dump
//     buffering) but log-query responses can be large, so 512.
//   - storage/metadata: colocated-in-one-bucket by default, so naming
//     a bucket is the only storage config.
//   - the self-URL and bucket placeholders resolve from PreProvision
//     tpl extensions; see Schema's SupportsPreProvision note.
var componentDefaultsJSON = func() []byte {
	defaults := map[string]any{
		"deployTo":  "lambda",
		"maxMemory": 512,
		// 300 s, not SC's 30. This is NOT about a slow handler: it is
		// the healthcheck tick, which fans out across every registered
		// service in every registered tenant and probes each one. The
		// operator of the fleet stack had already overridden 30 to 300
		// by hand, which is the clearest possible signal that the
		// default was wrong rather than that their workload is unusual.
		"timeout":  300,
		"metadata": map[string]any{"type": "colocated"},
		"storage":  map[string]any{"type": "s3"},
		// The permissions this component needs, declared BY THE
		// COMPONENT and granted BY THE TEMPLATE.
		//
		// This is the division the generic design turns on. The
		// component knows what it must be able to do — read logs,
		// read metrics, discover which resources exist, read and write
		// its own storage. It must NOT know how the chosen cloud
		// expresses that: an IAM policy here, folder roles on Yandex,
		// a ClusterRole on Kubernetes. So it names actions in the
		// template's own vocabulary under the template's key
		// (`cloudExtras.awsRoles` for aws-lambda) and the template
		// attaches them to the service's own identity. No access key
		// is created, so there is nothing to store, rotate or leak,
		// and the operator writes none of it.
		//
		// The read set is least-privilege, derived from what the
		// service actually calls rather than from a convenience
		// wildcard — a managed ReadOnlyAccess would grant read on
		// every service in the account, which is both more than this
		// needs and more than an operator should have to accept to get
		// a dashboard. There is no Put, Create or Delete outside the
		// component's own bucket: a dashboard that can change
		// infrastructure is a liability.
		"cloudExtras": map[string]any{
			// Function URL, not API Gateway. The APIGW path mounts the
			// lambda under a stage named "api", so every request
			// arrives path-prefixed and a SPA that serves its own
			// routes answers 404 for all of them — the stage-prefix
			// doubling that cost sc-component-backup a deploy cycle.
			"lambdaRoutingType": "function-url",
			// The healthcheck sweep's timer. The service exposes POST
			// /api/v1/scheduler/healthcheck/tick and runs the sweep
			// inline when called; nothing calls it on its own, and a
			// Lambda cannot hold a ticker (the runtime freezes
			// background goroutines when the handler returns). So the
			// timer has to be a platform resource — on aws-lambda, an
			// EventBridge rule built from this key.
			//
			// This block used to carry a ~25-line comment explaining
			// that it CANNOT live here, and the operator wrote it by
			// hand instead. That comment was a misdiagnosis; see
			// [schedulerTickRequest] for what actually went wrong on
			// 2026-11-19 and why the sentinel's POSITION is the whole
			// difference.
			"lambdaSchedules": []any{
				map[string]any{
					"name":       "hc-tick",
					"expression": "rate(5 minutes)",
					"request":    schedulerTickRequest("scheduled-healthcheck", "/api/v1/scheduler/healthcheck/tick"),
				},
			},
			"awsRoles": []string{
				// Storage — the component's own bucket: registry
				// records, deploy history, health results.
				"s3:PutObject", "s3:GetObject", "s3:ListBucket",
				"s3:DeleteObject", "s3:AbortMultipartUpload",

				// Logs. DescribeLogGroups/Streams resolve an inferred
				// log-group PREFIX to concrete groups, because the
				// Pulumi-generated function suffix is not derivable.
				// FilterLogEvents/GetLogEvents tail and search.
				// StartQuery/GetQueryResults/StopQuery run Insights for
				// what a filter cannot express.
				"logs:DescribeLogGroups", "logs:DescribeLogStreams",
				"logs:FilterLogEvents", "logs:GetLogEvents",
				"logs:StartQuery", "logs:StopQuery",
				"logs:GetQueryResults", "logs:DescribeQueries",

				// Metrics. ListMetrics discovers which dimensions
				// actually exist, so a metric the service never emitted
				// renders as absent rather than as an empty chart.
				"cloudwatch:GetMetricData", "cloudwatch:GetMetricStatistics",
				"cloudwatch:ListMetrics",

				// Discovery — resolve a stack name to its live
				// resources. Needed because the registry records what
				// SC deployed, while these answer what is actually
				// running.
				"lambda:ListFunctions", "lambda:GetFunction",
				"lambda:GetFunctionConfiguration", "lambda:ListTags",
				"ecs:ListClusters", "ecs:ListServices", "ecs:DescribeServices",
				"ecs:ListTasks", "ecs:DescribeTasks", "ecs:DescribeTaskDefinition",
			},
		},
		"env": map[string]any{
			// The service's own public URL, derived from
			// ingress.hostname at PreProvision. Lets the deployed
			// service log and display where it lives, and is the
			// callback base for the dispatched query worker in slice 2.
			"OBSERVATORY_SELF_URL": "$$sc:${resource:${name}.self-url}",
			// Storage wiring. STORAGE_* rather than OBSERVATORY_S3_*
			// on purpose: sc-plugin-aws's generic storage-block → env
			// translation already emits these names, and the backup
			// component lost a deploy cycle to exactly that mismatch
			// (its service demanded BACKUP_S3_BUCKET while the
			// platform emitted STORAGE_BUCKET, and the dispatch
			// silently fell back to a broken path).
			"STORAGE_BUCKET": "$$sc:${resource:${name}.storage-bucket}",
			"STORAGE_REGION": "$$sc:${resource:${name}.storage-region}",
			// go-embedb is the metadata engine, not an opt-in
			// experiment. The colocated S3 store it replaced served
			// read-modify-write upserts only from whichever warm
			// execution environment held the lease and answered
			// NotWritablePrimary(10107) from every other one, which is
			// not a tuning problem — it is a correctness one, and it
			// cost this component a generation of registry data. An
			// operator who leaves this unset should get the engine the
			// fleet actually runs.
			"USE_GOEMBEDB": "1",
			// The bucket go-embedb keeps its log + snapshots in: the
			// SAME bucket as the component's own data, under its own
			// prefix (GOEMBEDB_S3_PREFIX, defaulted in the service).
			// One bucket means one lifecycle policy and one set of
			// grants, and the `awsRoles` above already cover it.
			//
			// Derived from the same tpl extension STORAGE_BUCKET uses,
			// so the two cannot name different buckets — which was a
			// live hazard while this was a hand-written server.yaml
			// line, because a bare `storage: sc-observatory-data` grows
			// a `--<env>` suffix that an operator repeating the base
			// name by hand does not.
			"GOEMBEDB_S3_BUCKET": "$$sc:${resource:${name}.storage-bucket}",
		},
	}
	// The service image this plugin version ships with; see
	// [defaultImage]. Omitted for an unstamped build.
	if img := defaultImage(); img != "" {
		defaults["image"] = img
	}
	b, err := json.Marshal(defaults)
	if err != nil {
		// A bad defaults payload would otherwise cascade into every
		// Schema() RPC; fail at load instead.
		panic("sc-component-observatory: componentDefaultsJSON: " + err.Error())
	}
	return b
}()

// imageRepo is the Docker Hub repository this plugin's service image is
// published to. Kept beside [defaultImage] because the two are a pair:
// release.yml pushes `<imageRepo>:<tag>` and stamps `<tag>` into
// [version] in the same job, off the same CalVer value.
const imageRepo = "simplecontainer/sc-component-observatory"

// defaultImage is the service image that matches THIS plugin binary.
//
// The plugin and its service image are released together —
// `.github/workflows/release.yml` computes one CalVer tag, stamps it
// into `main.version` with `-ldflags -X`, and pushes
// `simplecontainer/sc-component-observatory:<that same tag>`. So the
// binary already knows the image it belongs with, and an operator who
// pins the plugin version has, by construction, pinned the image too.
//
// Defaulting it here is therefore not a convenience — it ENFORCES the
// lockstep. Two pins for one release is two chances to bump one and not
// the other, and a plugin running against a mismatched service image
// fails in whichever way the contract happened to change that week,
// with nothing in the config to point at.
//
// Returns "" for an unstamped build (`version` is "dev" for a local
// `go build`), so a developer gets SC's own "image required" error
// instead of a pull of a tag that was never pushed.
func defaultImage() string {
	if version == "" || version == "dev" {
		return ""
	}

	return imageRepo + ":" + version
}

// schedulerTickRequest renders the EventBridge target input that drives
// the healthcheck sweep.
//
// THE SENTINEL POSITION IS THE WHOLE TRICK, and it is why this default
// was previously believed impossible. `substituteString`
// (api `pkg/clouds/pulumi/api/defaults_interpolation.go:174`) returns
// the string UNCHANGED unless it BEGINS with `$$sc:`. So the obvious
// shape — the sentinel next to the token it guards, as in
// `"Bearer $$sc:${resource:${name}.scheduler-token}"` — is skipped in
// silence. That is exactly what shipped here on 2026-11-19: the rule
// was created, fired on time, and the tick answered 401 twice
// (10:56:13, 11:01:13) with the literal
// `Bearer $$sc:${resource:${name}.scheduler-token}` in its input.
//
// The conclusion drawn then was that SC resolves `${secret:NAME}`
// inside a schedule payload but not the tpl-extension form, and that a
// generic component therefore cannot ship a schedule at all. A ~25-line
// comment to that effect went into all three components (it was where
// this function now stands), and every operator has been writing this
// block by hand since. It was a misdiagnosis.
//
// With the sentinel at position 0 of the WHOLE payload: `${name}`
// substitutes, the sentinel is stripped, and core's post-PreProvision
// `ResolvePlaceholders` pass (`plugin_sidecars.go:1580`) walks into
// `cloudExtras.lambdaSchedules[].request` — a string inside a map
// inside a slice — and substitutes `${resource:…}` mid-string. No core
// change required.
//
// Constraint worth keeping in view: a schedule `name` longer than 14
// characters is rejected at APPLY, after preview has passed.
func schedulerTickRequest(requestID, path string) string {
	req := map[string]any{
		"requestId":  requestID,
		"httpMethod": http.MethodPost,
		"path":       path,
		"requestContext": map[string]any{
			"http": map[string]any{
				"path":     path,
				"method":   http.MethodPost,
				"protocol": "HTTP/1.1",
			},
		},
		"body": "{}",
		"headers": map[string]any{
			"Authorization": "Bearer ${resource:${name}.scheduler-token}",
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		// Unreachable: every value above is a marshallable literal.
		// Panic rather than ship a schedule whose input is empty — a
		// rule that fires every five minutes and 400s reads as "the
		// dashboard is broken".
		panic("sc-component-observatory: schedulerTickRequest: " + err.Error())
	}

	return pApi.DefaultsSentinel + string(b)
}

func (p *observatoryPlugin) Schema(_ context.Context, _ *scpluginv1.SchemaRequest) (*scpluginv1.SchemaResponse, error) {
	return &scpluginv1.SchemaResponse{
		PluginVersion:          "sc-component-observatory-" + version,
		ResourceTypes:          []string{observatoryApi.ResourceType},
		ComponentResourceTypes: []string{observatoryApi.ResourceType},
		ComputeProcessorTypes:  []string{observatoryApi.ResourceType},
		PulumiPlugins:          []*scpluginv1.PulumiPluginRequirement{},
		VmDrivers:              []*scpluginv1.VmDriverSpec{},

		ComponentDefaults: map[string][]byte{
			observatoryApi.ResourceType: componentDefaultsJSON,
		},

		// PreProvision must run BEFORE the compute template is
		// dispatched so its tpl extensions are available for the
		// `${resource:<name>.*}` placeholders in ComponentDefaults
		// above. Without this flag SC core never dials PreProvision and
		// the deployed service receives literal unresolved tokens in
		// its env — which looks like a working deploy right up until
		// the service cannot find its bucket.
		SupportsPreProvision: true,

		// Operator-config allowlist. SC core dual-uses this as the
		// allowed-top-level-keys list for the component config, so
		// EVERY key an operator may legitimately write must appear
		// here or parse-time validation rejects the whole stack with
		// "unknown or disallowed top-level key".
		//
		// Learned the hard way on sc-component-backup: `maxMemory` was
		// missing, so a config the plugin otherwise honoured was
		// refused at provision — and once allowlisted, it still did
		// not take effect until sc-plugin-aws's overlay learned to
		// write it through. Both halves are needed; both are in place
		// upstream now, so these are listed from commit one here.
		ComponentAllowedOverrides: map[string]*scpluginv1.StringList{
			observatoryApi.ResourceType: {Values: []string{
				// Template-side overlays.
				"image", "env", "ingressRoutes", "imagePullSecretRef", "cloudExtras",
				// Compute shape — operator wins over the defaults above.
				"maxMemory", "timeout",
				// Component-specific top-level keys.
				"storage", "metadata", "operatorPassword", "readers",
				// Which consumer environments may register as tenants.
				// Empty/absent allows all — the pre-filter behaviour.
				"tenantEnvironments",
				// The `op: "scheduler"` token the EventBridge rule
				// presents on the tick. Scoped to that one route, so the
				// credential in the rule's input can only fire a sweep.
				"schedulerToken",
				// The bearer the one-off S3 → go-embedb migration
				// endpoint demands. Bound through ComponentSecretEnvMap
				// below rather than written into `env`, so the token
				// that can rewrite the whole datastore lands in a
				// per-function SecretsManager entry instead of the
				// Lambda's environment. Set for the cutover, removed
				// afterwards — absent, the service never mounts the
				// route.
				"migrationToken",
				// Grandfather for the aws-lambda template's
				// per-Component resource naming (sc-plugin-aws#28): keeps
				// a Component deployed BEFORE that scoping on its
				// un-scoped names, because switching over is a pure
				// rename and Pulumi matches by name — so the plan would
				// create new resources and DELETE the live ones.
				//
				// This component has never deployed un-scoped, so it
				// will never need the flag. It is on the allowlist so an
				// operator who hits the same situation on a FUTURE
				// template-naming change has the same escape, and so the
				// key means the same thing on every Component rather
				// than being refused here and accepted next door. Here
				// only because SC core dual-uses this list as
				// allowed-top-level-keys; the key is read by the
				// template plugin, not by this one.
				"legacyResourceNames",
			}},
		},

		// Generic secret-env wire: tells the template plugin which env
		// var to bind each `${secret:...}` config field to on the
		// dispatched service. The template plugin binds these to
		// per-function SecretsManager secrets rather than plain env, so
		// plaintext never lands in a Lambda's environment — and the
		// service resolves the ARN at startup (secret_resolver.go).
		ComponentSecretEnvMap: map[string]*scpluginv1.StringMap{
			observatoryApi.ResourceType: {Entries: map[string]string{
				"signingKey":       "SERVICE_JWT_SECRET",
				"operatorPassword": "OBSERVATORY_OPERATOR_PASSWORD",
				"migrationToken":   "ADMIN_MIGRATION_TOKEN",
			}},
		},
	}, nil
}

// PreProvision lands derived values as tpl extensions.
//
// Everything computed here is deterministic from the operator's config
// — no AWS calls — so it cannot fail for environmental reasons and
// needs no error swallowing. That is a deliberate contrast with the
// backup component, whose PreProvision does a default-VPC lookup and
// has to tolerate that lookup failing.
func (p *observatoryPlugin) PreProvision(stdctx context.Context, req *scpluginv1.PreProvisionRequest) (resp *scpluginv1.PreProvisionResponse, err error) {
	rctx, err := runtime.NewContext(stdctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	resName := req.GetResourceName()
	env := req.GetHeader().GetEnvironment()
	p.progress.Info("pre-provision", fmt.Sprintf("observatory component %q (env %q)", resName, env))

	var raw map[string]any
	if b := req.GetComponentRaw(); len(b) > 0 {
		if jerr := json.Unmarshal(b, &raw); jerr != nil {
			return nil, fmt.Errorf("decode ComponentRaw: %w", jerr)
		}
	}
	if raw == nil {
		raw = map[string]any{}
	}

	tpl := map[string]string{
		"self-url":       observatory.SelfURLFromConfig(raw),
		"storage-bucket": storageBucket(raw, resName, env),
		"storage-region": storageRegion(raw),
		// The credential the deploy template's timer presents on the
		// healthcheck tick. Landed as a tpl extension rather than named
		// as a secret inside ComponentDefaults, because those defaults
		// ship to every operator and must not assume one stack's secret
		// names.
		"scheduler-token": schedulerToken(raw),
	}

	return runtime.RunPreProvision(
		stdctx, rctx,
		api.Stack{Name: req.GetHeader().GetStackName()},
		api.ResourceInput{Descriptor: &api.ResourceDescriptor{
			Type: req.GetResourceType(),
			Name: resName,
		}},
		pApi.ProvisionParams{},
		func(_ *sdk.Context, _ api.Stack, _ api.ResourceInput, collector pApi.ComputeContextCollector, _ pApi.ProvisionParams) error {
			collector.AddResourceTplExtension(resName, tpl)
			p.progress.Info("pre-provision", fmt.Sprintf(
				"observatory %q derived: self-url=%q bucket=%q region=%q",
				resName, tpl["self-url"], tpl["storage-bucket"], tpl["storage-region"],
			))
			return nil
		},
	)
}

// storageBucket resolves the operator's `storage:` declaration — a
// REFERENCE to another SC resource — to the physical bucket the
// deployed service uses. See observatory.BucketForStorageRef for the
// accepted shapes and why the resolution is a convention rather than a
// lookup.
func storageBucket(raw map[string]any, resName, env string) string {
	return observatory.BucketForStorageRef(raw, resName, env)
}

// schedulerToken is the operator-declared credential the deploy
// template's timer presents on the healthcheck tick.
//
// Returns "" when unset, and the schedule then carries an empty bearer
// and the tick 401s. That is deliberately LOUD rather than silently
// skipping the schedule: a dashboard whose health never updates looks
// identical to a healthy fleet, which is the failure this whole slice
// exists to remove. A 401 every five minutes in the service's own log
// is a thing an operator can find.
func schedulerToken(raw map[string]any) string {
	if v, ok := raw["schedulerToken"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// storageRegion defaults to eu-central-1, matching the fleet's primary.
// An operator in another region sets `storage.region` once.
func storageRegion(raw map[string]any) string {
	if st, ok := raw["storage"].(map[string]any); ok {
		if r, ok := st["region"].(string); ok && r != "" {
			return r
		}
	}
	return "eu-central-1"
}

// Provision is a near-no-op by design: Path A dispatches the
// operator-chosen compute template before this runs, so the service is
// already deployed. Kept so the RPC is answered rather than erroring,
// and so the deploy log records that the component was seen.
func (p *observatoryPlugin) Provision(stdctx context.Context, req *scpluginv1.ProvisionRequest) (resp *scpluginv1.ProvisionResponse, err error) {
	rctx, err := runtime.NewContext(stdctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)
	p.progress.Info("provision", fmt.Sprintf(
		"observatory component %q provisioned via the dispatched template (nothing further to do)",
		req.GetResourceName(),
	))
	return &scpluginv1.ProvisionResponse{}, nil
}

// ComputeProcess fires once per consumer that lists this component in
// `uses:`, and push-registers that deploy.
func (p *observatoryPlugin) ComputeProcess(stdctx context.Context, req *scpluginv1.ComputeProcessRequest) (resp *scpluginv1.ComputeProcessResponse, err error) {
	rctx, err := runtime.NewContext(stdctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	p.progress.Info("compute-process", fmt.Sprintf("resource=%q type=%q consumer=%q",
		req.GetResourceName(), req.GetResourceType(), req.GetHeader().GetStackName()))

	parent, err := runtime.ParentInfoFromRequest(req)
	if err != nil {
		return nil, fmt.Errorf("parent decode: %w", err)
	}
	deps, err := runtime.DependencyOutputsFromComputeProcessRequest(req)
	if err != nil {
		return nil, fmt.Errorf("deps decode: %w", err)
	}
	componentRaw, err := runtime.ComponentRawFromComputeProcessRequest(req)
	if err != nil {
		return nil, fmt.Errorf("component_raw decode: %w", err)
	}
	if componentRaw == nil {
		componentRaw = map[string]any{}
	}
	input := runtime.ResourceInputForComputeProcess(req, parent, componentRaw)

	// Environment gate — BEFORE any work, and before anything is sent.
	//
	// An SC parent stack can be cloned into short-lived isolated stacks
	// (the Forge fleet spins one per agent-workflow run). Those blocks
	// inherit the base config with a YAML merge key, and a merge key
	// cannot DELETE an inherited entry — so an isolated deploy inherits
	// `uses: [… my-observatory]` with no way to opt out from the YAML
	// side. Left alone the dashboard fills with rows for stacks that
	// were destroyed hours ago, all `health: unknown`, which reads
	// exactly like real services that stopped answering.
	//
	// Skipping is NOT silent: the deploy log says which environment was
	// refused and what the allowlist is, so an operator who adds a real
	// environment and forgets to list it finds out from the deploy that
	// should have registered it, rather than from an empty dashboard
	// weeks later.
	consumerEnv := consumerEnvironment(req, parent)
	if allowed := observatory.TenantEnvironmentsFromConfig(componentRaw); !observatory.EnvMayRegister(allowed, consumerEnv) {
		p.progress.Info("compute-process", fmt.Sprintf(
			"skipping tenant registration for %q: environment %q is not in tenantEnvironments %v",
			req.GetHeader().GetStackName(), consumerEnv, allowed,
		))
		return &scpluginv1.ComputeProcessResponse{}, nil
	}

	// The consuming stack's own client descriptor (api#639). Raw/map
	// form, not typed: this is FORWARDED to the service rather than
	// interpreted, and round-tripping through a typed struct would
	// silently drop any field this plugin's pinned api version does not
	// know about yet.
	//
	// A decode failure is logged and ignored rather than failing the
	// consumer's deploy — the dashboard losing one field of context is
	// not worth blocking a release over.
	clientCfg, cErr := runtime.ClientDescriptorRawFromComputeProcessRequest(req)
	if cErr != nil {
		p.progress.Warn("compute-process", "client descriptor decode: "+cErr.Error()+" — registering without the consumer's config")
		clientCfg = nil
	}

	processor := observatory.NewProcessor(observatory.ProcessorConfig{
		Progress:     p.progress,
		ClientConfig: clientCfg,
		// Deploy identity off the bus header (api#640): the version the
		// CI action computed, the full commit SHA, and the repository.
		// Zero value against an older core, which is the pre-#640
		// behaviour — version then comes from the health probe.
		DeployIdentity: runtime.DeployIdentityFromHeader(req.GetHeader()),
	})

	// WithIncomingEnvVars seeds the collector from the shared compute
	// context, so this processor sees env emitted by PRECEDING `uses:`
	// entries — which is where VERSION, GITHUB_SHA and any published
	// telemetry handles come from. Hence the documented requirement
	// that `my-observatory` be listed LAST.
	resp, _, err = runtime.RunComputeProcess(
		stdctx, rctx,
		api.Stack{Name: req.GetHeader().GetStackName()}, input,
		pApi.ProvisionParams{Log: logger.New(), ParentStack: parent, ResourceOutputs: deps},
		processor.Process,
		runtime.WithIncomingEnvVars(req),
	)
	return resp, err
}

// PreDestroy fires when the component's OWN resource is destroyed —
// NOT when a consumer is.
//
// There is no "consumer destroyed" event on the plugin bus, which is
// why the service ships DELETE /api/v1/services/{stack} for operators
// and a staleness badge for everything else. Nothing to clean up here:
// observatory mints no credentials and the registry lives in the
// operator's own bucket, which they may want to keep.
func (p *observatoryPlugin) PreDestroy(_ context.Context, req *scpluginv1.PreDestroyRequest) (*scpluginv1.PreDestroyResponse, error) {
	// PreDestroyRequest carries the resource TYPE and the stack header,
	// but no resource name — so the log line identifies the component by
	// type + stack rather than inventing a name.
	p.progress.Info("pre-destroy", fmt.Sprintf(
		"observatory component (type %q, stack %q): registry data is left in place in the operator's bucket; consumers are not notified (no consumer-destroy event exists on the plugin bus)",
		req.GetResourceType(), req.GetHeader().GetStackName(),
	))
	return &scpluginv1.PreDestroyResponse{}, nil
}

// LogStream drains the progress Publisher onto the gRPC stream. SC core
// opens one of these per plugin after Schema() returns; without it the
// Info/Warn lines this plugin emits never reach the deploy log, which
// would make a failed registration invisible.
func (p *observatoryPlugin) LogStream(_ *scpluginv1.LogStreamRequest, stream scpluginv1.SCCloudPlugin_LogStreamServer) error {
	ctx := stream.Context()
	for {
		batch, ok := p.progress.Drain()
		if !ok {
			return nil
		}
		for _, ev := range batch {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// Keeps the api package referenced even if a future edit drops its
// only other use, mirroring sc-component-backup.
var _ = api.Credentials{}

func main() {
	impl := &observatoryPlugin{progress: scPlugin.NewPublisher(0)}
	scPlugin.Serve(scPlugin.ServerConfig{
		Impl: impl,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "sc-component-observatory",
			Level:  hclog.Warn,
			Output: os.Stderr,
		}),
	})
}

// consumerEnvironment resolves the environment the CONSUMER is being
// deployed to.
//
// The header is authoritative. ParentInfo.StackEnv is the fallback for
// a dispatch shape that leaves the header's env empty, and the stack
// name's `--<env>` suffix is the last resort — it is what the service
// itself derives for display, so a value found there is at least
// consistent with what an operator sees.
func consumerEnvironment(req *scpluginv1.ComputeProcessRequest, parent *pApi.ParentInfo) string {
	if e := strings.TrimSpace(req.GetHeader().GetEnvironment()); e != "" {
		return e
	}
	if parent != nil {
		if e := strings.TrimSpace(parent.StackEnv); e != "" {
			return e
		}
	}
	if stack := req.GetHeader().GetStackName(); stack != "" {
		if i := strings.LastIndex(stack, "--"); i >= 0 && i+2 < len(stack) {
			return stack[i+2:]
		}
	}
	return ""
}
