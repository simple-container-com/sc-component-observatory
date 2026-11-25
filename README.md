# sc-component-observatory

A reusable [Simple Container](https://github.com/simple-container-com/api) **Component**
that answers two questions about an operator's fleet:

- **What is deployed where?** Service, environment, version, git commit, deploy time.
- **Is it healthy?** Logs and metrics for those services, without visiting four cloud consoles.

Resource type: **`sc-observatory`**.

## Why not just use a cloud console

Because SC deploys to several clouds. `simple-container-com/observatory` — the ancestor of
this component — answers both questions well, but only for AWS: it is wired directly to
CloudWatch Logs and ECS. SC also targets Yandex Serverless Containers, Kubernetes/GKE and
plain VM compose, so a dashboard that only sees one cloud is not a platform feature.

This component keeps that feature set and abstracts the cloud away behind a provider
interface, with the permissions provisioned per deploy template rather than hand-written.

## Design

Full design, including the friction register from the first SC Component's fleet roll:
[`forge/docs/design/2026-11-18/sc-component-observatory/`](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-11-18/sc-component-observatory).

Two rules carry most of the weight:

1. **Observatory never derives a telemetry handle.** The plugin that created a resource is
   the only thing that reliably knows its log group, metric namespace or container id. Until
   every template plugin publishes them, handles are inferred from deterministic naming and
   the record is flagged `inferred` — a guess that renders *as* a guess.
2. **Observatory never writes an IAM policy.** Read permissions come from the cloud's own
   plugin via a peer-invoke capability, the same way the backup component gets scoped
   database users. Same-cloud is zero-config; cross-cloud costs one secret per foreign cloud,
   which the UI states rather than rendering empty charts.

## Layout

```
api/        operator-facing config types + validation (doc-only; dispatch reads .Raw)
plugin/     the SC plugin: Schema, PreProvision, Provision, ComputeProcess, PreDestroy
service/    the deployed service: HTTP API, service registry, telemetry providers, SPA
```

## Status

Slice 1 in progress — the service registry.

| Slice | Scope | State |
|---|---|---|
| 1 | Registry: push-register on deploy, list/get/delete, deploy timeline | in progress |
| 2 | AWS provider — logs (CloudWatch + Insights, dispatched for long ranges) | next |
| 3 | AWS provider — metrics | |
| 4 | Template plugins publish telemetry handles + grant reader creds; drop inference | |
| 5 | Deploy timeline UI | |
| 6 | Yandex Cloud provider | |
| 7 | Kubernetes provider | |

## Operator config

Target is under ten lines; anything derivable is derived by the plugin.

```yaml
my-observatory:
  type: sc-observatory
  config:
    deployTo: lambda                   # picks the template; everything
                                       # derivable is derived from it
    ingress:
      hostname: observatory.example.com
    storage: observatory-data          # name of another SC resource
    signingKey:       "${secret:observatory-signing-key}"
    operatorPassword: "${secret:observatory-operator-password}"
    schedulerToken:   "${secret:observatory-scheduler-token}"
    # Only during the S3 -> go-embedb cutover; see Storage below.
    # Remove it afterwards and the migration route is not mounted.
    # migrationToken: "${secret:observatory-migration-token}"
```

Nine lines, and every one of them is a decision only the operator can make: which template,
which hostname, which bucket, which credentials. Everything else comes from
`deployTo: lambda`:

| Derived | Value |
|---|---|
| `image` | `simplecontainer/sc-component-observatory:<this plugin's version>` — the plugin and the image are released together off one CalVer tag, so pinning the plugin pins the image |
| `maxMemory` / `timeout` | `512` / `300` — measured; SC's own defaults are 128/30 and the healthcheck sweep does not fit in them |
| `cloudExtras.lambdaRoutingType` | `function-url` |
| `cloudExtras.awsRoles` | the least-privilege read set the service actually calls, granted to the function's own identity — no access key is created |
| `cloudExtras.lambdaSchedules` | the `hc-tick` rule, `rate(5 minutes)`, presenting your `schedulerToken` |
| `storage.type` / `metadata.type` | `s3` / `colocated` |
| `env.STORAGE_BUCKET` / `STORAGE_REGION` | from your `storage:` block |
| `env.USE_GOEMBEDB` / `GOEMBEDB_S3_BUCKET` | `1`, and the same bucket under its own prefix |

**Overriding any of them is one key in `config:`** — operator wins per key. Two cautions:
a list value is taken **wholesale**, not merged, so writing your own `awsRoles` replaces
the component's set rather than adding to it; and `storage:` has two shapes that do not
agree — a bare `storage: observatory-data` grows a `--<env>` suffix, while
`storage: {bucket: observatory-data}` is taken verbatim.

The schedule in particular used to be a ~30-line block every operator pasted by hand,
because an early failure was read as "SC cannot resolve `${resource:...}` inside a schedule
payload". It can; the sentinel just has to be at position 0 of the payload rather than
beside the token. See `schedulerTickRequest` in `plugin/cmd/sc-component-observatory/main.go`.

`schedulerToken` is an HS256 JWT signed with `signingKey`, carrying
`{"op":"scheduler"}`. The tick requires that op, so the credential sitting in the
EventBridge rule's input — readable by anyone with `events:DescribeRule` in the account —
can fire a sweep and nothing else: no registry read, no tenant registration, no other
route. Mint it once:

```python
jwt.encode({"op": "scheduler", "sub": "eventbridge-healthcheck",
            "iat": now, "nbf": now, "exp": now + 10*365*24*3600},
           signing_key, algorithm="HS256")
```

Long-lived on purpose: an EventBridge rule input has no rotation hook, so a short expiry
stops the sweep and presents as "the dashboard went stale" rather than as an auth error
anyone would look for. The `op` bound is what limits the damage instead.

Consumers opt in with one line. It must be **last** in `uses:` — telemetry discovery only
sees what preceding entries published:

```yaml
      uses:
        - mongodb
        - my-observatory
```

## Storage

Observatory stores its own registry and deploy history, never telemetry: logs and metrics
are read from the target cloud at query time. The backend is pluggable — S3 (including any
S3-compatible gateway), GCS, or a filesystem path (a Kubernetes PVC, an EFS mount, a VM
directory) — with structured records colocated as JSON in the same bucket by default, so a
bucket name is the only storage config required.

### Backends

| Backend | Env flag | State |
|---|---|---|
| **S3** (current) | `USE_GOEMBEDB` unset or `0` | Default. S3Store over the configured bucket (`STORAGE_BUCKET`), JSON records under `_meta/`. |
| **go-embedb** (new) | `USE_GOEMBEDB=1` | MongoStore over an embedded go-embedb (ferretdb + sqlite + S3 active-active replication). Collections: `services`, `deploys`, `deploy_configs`, `health`, `kv`. See [`forge/docs/design/2026-11-22/go-embedb-active-active-s3/`](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-11-22/go-embedb-active-active-s3). |

### Building against go-embedb (a private dependency)

go-embedb is a **private** fork of FerretDB, published under the upstream module path, so
`service/go.mod` routes it with

```
replace github.com/FerretDB/FerretDB => github.com/simple-container-com/go-embedb v0.0.0-…
```

Two properties of that line are load-bearing, and the first one took this repo's `main` red
once already (`d0c3c19`, reverted by `#23`):

- **It must be a module path + pseudo-version, never a filesystem path.** A sibling-path
  replace (`=> ../../go-embedb`) resolves only on a machine that happens to have the fork
  checked out next door. And it fails in `go mod download` — while the module graph is
  being loaded, *before* any build tag or env var is read — so `USE_GOEMBEDB` defaulting to
  `0` gates nothing: the S3 path becomes unbuildable too.
- **A filesystem replace never validates the pseudo-version string**, so a wrong timestamp
  in it stays invisible until the day the path is swapped for a module path.

Because the fork is private, a build needs a read credential:

| Where | How |
|---|---|
| **CI** (`.github/workflows/release.yml`) | `sc secrets reveal --force` materialises `.sc/deploy-keys/go-embedb.pem` from this repo's SC registry, and `docker/build-push-action`'s `secret-files:` mounts it into the `go-builder` stage. The mount is not a layer and not a build-arg, so the key reaches neither the image nor the log. |
| **Locally** | `sc secrets reveal --force`, then `GOPRIVATE='github.com/simple-container-com/*' GIT_SSH_COMMAND='ssh -i .sc/deploy-keys/go-embedb.pem -o IdentitiesOnly=yes' go mod download`. `GOPRIVATE` is required: without it the public proxy tries the fetch, cannot read a private repo, and reports git's auth failure wrapped as a misleading `invalid version`. |

The key itself is **SC-encrypted in `.sc/secrets.yaml`**, which is committed; the plaintext
half and `.sc/cfg.*.yaml` are gitignored. The registry is encrypted to the fleet's shared
recipient set, so the single **org-level `SC_CONFIG`** decrypts it with no per-repo GitHub
secret — see the fleet convention in `forge/CLAUDE.md` §"Secret management".

### Migration (S3 → go-embedb)

Opt-in, operator-driven, reversible.

**The flag goes on FIRST.** The go-embedb database *is* the migration sink, and
it is only booted under `USE_GOEMBEDB=1` — with the flag off there is nothing to
migrate into, and the endpoint answers 500 naming what is missing. So the order
is **flip → dry-run → migrate**, and there is a window in the middle where the
service serves an empty store while S3 still holds everything.

1. **Redeploy with the flag on, replication configured, and the migration token
   set:**
   ```yaml
   my-observatory:
     config:
       migrationToken: "${secret:observatory-migration-token}"   # remove after cutover
       env:
         USE_GOEMBEDB: "1"
         GOEMBEDB_S3_BUCKET: "<replication-bucket>"
   ```
   `GOEMBEDB_S3_BUCKET` is **hard-required on Lambda**: the service refuses to
   boot with `GOEMBEDB_S3_BUCKET is required on Lambda …`. Without a shared
   bucket, each warm container runs its own `/tmp`-backed store, up/down edge
   state resets across cold starts, and the alerting fires spurious edges —
   the exact shape this component exists to detect. The execution role needs
   read/write on that bucket prefix.

   `migrationToken` is bound through the component's secret-env wire, so it lands
   in a per-function SecretsManager entry and the service resolves the ARN at
   startup. Do **not** write `ADMIN_MIGRATION_TOKEN` under `env:` — that puts the
   credential that can rewrite the whole datastore in the Lambda's environment.

2. **The empty-store window opens here.** The dashboard lists nothing until step
   4 completes. Nothing is destroyed: S3 is untouched and the migration reads it
   read-only. Observatory's state is health ticks and deploy records, so the
   window costs visibility rather than data.

3. Dry-run — lists keys, counts both sides, confirms source + sink reachable, no
   write:
   ```sh
   curl -X POST \
     -H "X-Admin-Token: $TOKEN" \
     "https://<observatory-url>/admin/migrate-s3-to-goembedb?dry_run=1"
   ```
4. Wet run — upserts every S3 object into the embed:
   ```sh
   curl -X POST \
     -H "X-Admin-Token: $TOKEN" \
     "https://<observatory-url>/admin/migrate-s3-to-goembedb"
   ```
   Response `verified: true` means every S3 key landed. Rerun is idempotent (upsert).
5. Verify: the dashboard lists services again;
   `observatory_store_backend{backend="goembedb"} == 1` on `/metrics`; a **new**
   health tick appears in the embed.
6. **Remove `migrationToken` and redeploy.** Unset, the route is not mounted at
   all — that is the steady state, not a hardened one.

### Rollback

- Set `USE_GOEMBEDB=0` and redeploy. S3Store reads the pre-migration snapshot. Any writes made during the goembedb window are lost (they are in Mongo only, not back-copied to S3) — acceptable per the cutover decision.
- Full rollback: `sc destroy` the stack, `aws s3 rm backup-archives--<env>/sc-component-observatory--<env>/ --recursive`, redeploy from the previous commit.

### Operational metrics

Prometheus text at `GET /metrics`, unauthenticated.

- `observatory_store_backend{backend="s3"|"goembedb"}` — 1 for the active backend, 0 for the other. Alert if this disagrees with the configured `USE_GOEMBEDB`.
- Process + go-embedb replication series when `USE_GOEMBEDB=1` (slot heartbeats, write-concern waits, HLC drift — see the go-embedb operator guide).

### Environment knobs

| Env | Effect | Required when |
|---|---|---|
| `USE_GOEMBEDB` | `1` → MongoStore, anything else → S3Store. | Opt-in; default is S3. |
| `GOEMBEDB_S3_BUCKET` | Shared replication bucket (e.g. `backup-archives--staging`). | `USE_GOEMBEDB=1`. **Hard-required on Lambda** — unset refuses to boot with `GOEMBEDB_S3_BUCKET is required on Lambda …`. On ECS / VM / local smoke it is optional (fall back to single-node sqlite). |
| `GOEMBEDB_S3_PREFIX` | Key namespace in the bucket. | Optional. Default `sc-component-observatory/`. |
| `GOEMBEDB_S3_REGION` | AWS region for the bucket. | Optional; falls back to `AWS_REGION`. |
| `GOEMBEDB_REPLSET_NAME` | Replica-set label. | Optional. Default `observatory`. |
| `GOEMBEDB_SQLITE_DIR` | Local SQLite directory. | Optional. Default `/tmp/goembedb`. |
| `GOEMBEDB_ACK_WAIT_LIVE_ONLY` | `1` forces Bug 13 Lambda semantics on. | Auto-set on Lambda; explicit only for non-Lambda smokes. |
| `GOEMBEDB_HEARTBEAT_TTL` | Slot heartbeat TTL. | Optional. Defaults to `3m` on Lambda. |
| `GOEMBEDB_MAX_REPLICAS` | Hard cap on writer slots. | Optional. Defaults to `10` on Lambda. |
| `ADMIN_MIGRATION_TOKEN` | Enables `/admin/migrate-s3-to-goembedb`. Emitted by the plugin from the `migrationToken` config key, so it arrives as a SecretsManager ARN and is resolved at startup — do not set it under `env:` by hand. | Only during cutover. Unset to disable the route entirely. |

## License

MIT
