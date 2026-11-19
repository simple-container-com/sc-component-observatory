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
    deployTo: lambda
    image: simplecontainer/sc-component-observatory:v2026.11.xx
    ingress:
      hostname: observatory.example.com
    storage: observatory-data          # name of another SC resource
    signingKey:       "${secret:observatory-signing-key}"
    operatorPassword: "${secret:observatory-operator-password}"
    schedulerToken:   "${secret:observatory-scheduler-token}"
    # REQUIRED, and the one block that is not derivable. Without it
    # nothing ever probes: the service runs the sweep inline when its
    # tick endpoint is called and nothing calls it on its own, and a
    # Lambda cannot hold a ticker. Retention rides the same tick.
    #
    # It lives here rather than in the component's defaults because SC
    # resolves `${secret:NAME}` inside a schedule request but not the
    # `${resource:...}` form, and a generic component cannot hardcode
    # one stack's secret name. Paste as-is; only the secret name and the
    # interval are yours to change.
    cloudExtras:
      lambdaSchedules:
        - name: hc-tick                # <= 14 chars, rejected at APPLY
          expression: "rate(5 minutes)"
          request: |-
            {
              "requestId": "scheduled-healthcheck",
              "httpMethod": "POST",
              "path": "/api/v1/scheduler/healthcheck/tick",
              "requestContext": {
                "http": {
                  "path": "/api/v1/scheduler/healthcheck/tick",
                  "method": "POST",
                  "protocol": "HTTP/1.1"
                }
              },
              "body": "{}",
              "headers": {
                "Authorization": "Bearer ${secret:observatory-scheduler-token}"
              }
            }
```

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

## License

MIT
