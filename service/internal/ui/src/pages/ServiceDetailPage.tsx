import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, ExternalLink } from "lucide-react";
import { api, describeError } from "@/api/client";
import type { ConfigChange, ConfigDiff, DeployEvent } from "@/api/types";
import { ErrorBox, HealthBadge, Pill, Spinner, exact, relTime } from "@/components/bits";

export function ServiceDetailPage() {
  const { stack = "" } = useParams();
  const q = useQuery({
    queryKey: ["service", stack],
    queryFn: () => api.service(stack),
    refetchInterval: 30_000,
  });

  const back = (
    <Link to="/" className="inline-flex items-center gap-1.5 text-sm text-muted hover:text-fg">
      <ArrowLeft className="h-3.5 w-3.5" /> All services
    </Link>
  );

  if (q.isLoading) return <div className="space-y-4">{back}<Spinner /></div>;
  if (q.error)
    return (
      <div className="space-y-4">
        {back}
        <ErrorBox message={describeError(q.error)} onRetry={() => void q.refetch()} />
      </div>
    );

  const d = q.data!;
  const s = d.service;
  const regions = Object.entries(d.health ?? {});

  return (
    <div className="space-y-5">
      {back}

      <div>
        <div className="flex flex-wrap items-center gap-2">
          <h1 className="text-xl font-semibold">{s.service || s.stack}</h1>
          {s.env && <Pill>{s.env}</Pill>}
          <HealthBadge status={s.health} title={s.health_at ? `Last probe ${exact(s.health_at)}` : "Never probed"} />
        </div>
        <p className="mt-1 font-mono text-xs text-faint">{s.stack}</p>
      </div>

      {/* Deploy facts — the core question this dashboard answers. */}
      <div className="card p-4">
        <h2 className="text-sm font-medium">Current deploy</h2>
        <dl className="mt-3 grid grid-cols-2 gap-3 text-sm md:grid-cols-4">
          <Fact
            label="Version"
            value={s.version || "—"}
            mono
            /* Say WHICH version this is. A deploy-time value and a
               probe-time one look identical, and their disagreeing is
               the interesting case. */
            title={
              s.version_source === "probe"
                ? `${s.version} — reported by the service itself (no version was recorded at deploy)`
                : s.live_version && s.live_version !== s.version
                  ? `Deployed ${s.version}, but the service reports ${s.live_version}. The deploy may not have replaced the running code.`
                  : s.version
            }
          />
          <Fact label="Commit" value={s.short_commit || "—"} mono title={s.commit} />
          <Fact label="Deployed" value={relTime(s.deployed_at)} title={exact(s.deployed_at)} />
          <Fact label="First seen" value={relTime(s.first_seen_at)} title={exact(s.first_seen_at)} />
          <Fact label="Cloud" value={s.cloud || "—"} />
          <Fact label="Template" value={s.template || "—"} />
          <Fact label="Health path" value={s.health_path || "/healthz"} mono />
          <Fact label="Regions" value={s.regions.length ? s.regions.join(", ") : "—"} />
          {/* Only rendered when the bus carried it (api#640); absent
              against an older SC core, where a dash would read as "no
              repository" rather than "not known". */}
          {s.repository && <Fact label="Repository" value={s.repository} mono />}
        </dl>
        {s.endpoint_url && (
          <a
            href={s.endpoint_url}
            target="_blank"
            rel="noreferrer"
            className="mt-3 inline-flex items-center gap-1.5 text-xs text-accent hover:underline"
          >
            {s.endpoint_url} <ExternalLink className="h-3 w-3" />
          </a>
        )}
      </div>

      {/* Per-region health. Shown separately from the summary badge
          because a partial outage is the case a single status hides. */}
      {regions.length > 0 && (
        <div className="card p-4">
          <h2 className="text-sm font-medium">Health by region</h2>
          <ul className="mt-3 space-y-2">
            {regions.map(([region, res]) => (
              <li key={region || "default"} className="flex flex-wrap items-center gap-3 text-sm">
                <HealthBadge status={res.status} />
                <span className="font-mono text-xs">{region || "default"}</span>
                {res.http_status ? <span className="text-xs text-muted">HTTP {res.http_status}</span> : null}
                {res.latency_ms != null ? <span className="text-xs text-muted tnum">{res.latency_ms} ms</span> : null}
                <span className="text-xs text-faint" title={exact(res.at)}>{relTime(res.at)}</span>
                {res.error && <span className="font-mono text-xs text-danger">{res.error}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}

      <DeployTimeline stack={s.stack} fallback={d.deploys} />

      {/* Logs + metrics are the next slices. Stubbed EXPLICITLY rather
          than shown as empty panels: an empty chart reads as "no
          traffic", which is exactly the silent-degradation failure this
          component is supposed to avoid. */}
      <div className="grid gap-4 md:grid-cols-2">
        <Stub
          title="Logs"
          ready={s.telemetry_ready}
          inferred={s.inferred}
          sources={s.sources.filter((x) => x.kind.includes("log")).map((x) => x.ref)}
        />
        <Stub
          title="Metrics"
          ready={s.telemetry_ready}
          inferred={s.inferred}
          sources={s.sources.filter((x) => !x.kind.includes("log")).map((x) => x.ref)}
        />
      </div>

      {/* Full client config, as deployed. */}
      {s.has_client_config && (
        <details className="card p-4">
          <summary className="cursor-pointer text-sm font-medium">Deployed client config</summary>
          <pre className="mt-3 max-h-96 overflow-auto rounded bg-bg p-3 font-mono text-[11px] leading-relaxed text-muted">
            {JSON.stringify(d.client_config, null, 2)}
          </pre>
        </details>
      )}
    </div>
  );
}

/**
 * Deploy timeline with, per row, what changed since the deploy before
 * it.
 *
 * Its own query: the diff costs the backend one snapshot read per row,
 * so the detail payload carries the timeline WITHOUT diffs and this
 * fetches the annotated version alongside it. `fallback` is that
 * diff-less list — if the diff query fails, the timeline still renders
 * and the failure is reported in place of the diffs, rather than the
 * whole card disappearing over an additive feature.
 */
function DeployTimeline({ stack, fallback }: { stack: string; fallback: DeployEvent[] }) {
  const q = useQuery({
    queryKey: ["deploys", stack],
    queryFn: () => api.deploys(stack),
    refetchInterval: 60_000,
  });
  const rows = q.data?.deploys ?? fallback;

  return (
    <div className="card p-4">
      <div className="flex items-center gap-2">
        <h2 className="text-sm font-medium">Deploy history</h2>
        {q.isFetching && !q.data && <span className="text-xs text-faint">loading diffs…</span>}
      </div>

      {q.error && (
        <p className="mt-2 text-xs text-warn">
          Diffs unavailable: {describeError(q.error)}
        </p>
      )}

      {rows.length === 0 ? (
        <p className="mt-2 text-sm text-muted">No deploys recorded yet.</p>
      ) : (
        <ul className="mt-3 divide-y divide-line/60">
          {rows.map((ev) => (
            <DeployRow key={ev.id} ev={ev} />
          ))}
        </ul>
      )}
    </div>
  );
}

function DeployRow({ ev }: { ev: DeployEvent }) {
  const diff = ev.diff;
  const n = diff ? countChanges(diff) : 0;

  const header = (
    <div className="flex flex-wrap items-center gap-3 py-2 text-sm">
      <span className="font-mono text-xs">{ev.version}</span>
      {ev.previous_version && <span className="text-xs text-faint">from {ev.previous_version}</span>}
      {ev.commit && <span className="font-mono text-xs text-faint">{ev.commit.slice(0, 7)}</span>}
      <DiffSummary diff={diff} count={n} />
      <span className="ml-auto text-xs text-muted tnum" title={exact(ev.deployed_at)}>
        {relTime(ev.deployed_at)}
      </span>
    </div>
  );

  // Nothing to expand when there is no diff object at all (the
  // diff-less fallback list). An empty <details> that opens onto
  // nothing is worse than a plain row.
  if (!diff) return <li>{header}</li>;

  return (
    <li>
      <details className="group">
        <summary className="cursor-pointer list-none">{header}</summary>
        <div className="pb-3 pl-1">
          {diff.unavailable ? (
            /* Never an empty diff: "we have no snapshot" and "nothing
               changed" are different answers and an operator acts
               differently on each. */
            <p className="text-xs text-faint">{diff.unavailable}</p>
          ) : n === 0 ? (
            <p className="text-xs text-muted">
              No change in the client config — same settings, new build.
            </p>
          ) : (
            <div className="space-y-3">
              <ChangeGroup label="Environment" changes={diff.env} />
              <ChangeGroup label="Compute" changes={diff.compute} />
              <ChangeGroup label="Settings" changes={diff.settings} />
              <ChangeGroup label="Artifact" changes={diff.artifact} />
              {diff.truncated && (
                <p className="text-xs text-warn">
                  This diff was truncated — some changes are not shown.
                </p>
              )}
            </div>
          )}
        </div>
      </details>
    </li>
  );
}

function DiffSummary({ diff, count }: { diff?: ConfigDiff; count: number }) {
  if (!diff) return null;
  if (diff.unavailable) {
    return (
      <Pill title={diff.unavailable}>no diff</Pill>
    );
  }
  if (count === 0) return <span className="text-xs text-faint">no config change</span>;

  const parts: string[] = [];
  if (diff.env.length) parts.push(`${diff.env.length} env`);
  if (diff.compute.length) parts.push(`${diff.compute.length} compute`);
  if (diff.settings.length) parts.push(`${diff.settings.length} settings`);
  if (diff.artifact.length) parts.push(`${diff.artifact.length} artifact`);
  return <Pill tone="accent">{parts.join(" · ")}</Pill>;
}

function ChangeGroup({ label, changes }: { label: string; changes: ConfigChange[] }) {
  if (changes.length === 0) return null;
  return (
    <div>
      <h3 className="text-xs font-medium text-faint">{label}</h3>
      <ul className="mt-1 space-y-1">
        {changes.map((c) => (
          <li key={c.path} className="font-mono text-[11px] leading-relaxed">
            <span className={kindColor(c.kind)} title={c.kind}>
              {kindMark(c.kind)}
            </span>{" "}
            <span className="text-fg" title={c.path}>
              {c.key}
            </span>
            {c.secret && (
              <span className="ml-1.5 font-sans text-[10px] text-warn" title="Redacted at the source; its fingerprint changed, so the value was rotated">
                secret
              </span>
            )}
            <span className="text-muted">
              {c.kind === "added" ? (
                <> → {c.current}</>
              ) : c.kind === "removed" ? (
                <> {c.previous} →</>
              ) : (
                <> {c.previous} → {c.current}</>
              )}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function countChanges(d: ConfigDiff): number {
  return d.env.length + d.compute.length + d.settings.length + d.artifact.length;
}

function kindMark(kind: string): string {
  return kind === "added" ? "+" : kind === "removed" ? "−" : "~";
}

function kindColor(kind: string): string {
  return kind === "added" ? "text-ok" : kind === "removed" ? "text-danger" : "text-accent";
}

function Fact({ label, value, mono, title }: { label: string; value: string; mono?: boolean; title?: string }) {
  return (
    <div>
      <dt className="text-xs text-faint">{label}</dt>
      <dd className={mono ? "font-mono text-xs" : "text-sm"} title={title}>
        {value}
      </dd>
    </div>
  );
}

function Stub({ title, ready, inferred, sources }: { title: string; ready: boolean; inferred: boolean; sources: string[] }) {
  return (
    <div className="card p-4">
      <div className="flex items-center gap-2">
        <h2 className="text-sm font-medium">{title}</h2>
        {inferred && (
          <Pill tone="warn" title="These handles were guessed from naming conventions, not published by the plugin that owns the resource">
            inferred
          </Pill>
        )}
      </div>
      {!ready ? (
        <p className="mt-2 text-sm text-muted">
          No telemetry handles for this service — nothing to query. Its template did not publish any and none could be
          derived.
        </p>
      ) : (
        <>
          <p className="mt-2 text-sm text-muted">Query UI lands in a later slice. Handles discovered:</p>
          <ul className="mt-2 space-y-1">
            {sources.length === 0 && <li className="text-xs text-faint">none of this kind</li>}
            {sources.map((ref) => (
              <li key={ref} className="truncate font-mono text-xs text-faint" title={ref}>
                {ref}
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}
