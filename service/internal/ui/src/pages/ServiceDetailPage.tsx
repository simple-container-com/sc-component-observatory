import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, ExternalLink } from "lucide-react";
import { api, describeError } from "@/api/client";
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
          <Fact label="Version" value={s.version || "—"} mono />
          <Fact label="Commit" value={s.short_commit || "—"} mono title={s.commit} />
          <Fact label="Deployed" value={relTime(s.deployed_at)} title={exact(s.deployed_at)} />
          <Fact label="First seen" value={relTime(s.first_seen_at)} title={exact(s.first_seen_at)} />
          <Fact label="Cloud" value={s.cloud || "—"} />
          <Fact label="Template" value={s.template || "—"} />
          <Fact label="Health path" value={s.health_path || "/healthz"} mono />
          <Fact label="Regions" value={s.regions.length ? s.regions.join(", ") : "—"} />
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

      {/* Deploy timeline. */}
      <div className="card p-4">
        <h2 className="text-sm font-medium">Deploy history</h2>
        {d.deploys.length === 0 ? (
          <p className="mt-2 text-sm text-muted">No deploys recorded yet.</p>
        ) : (
          <ul className="mt-3 divide-y divide-line/60">
            {d.deploys.map((ev) => (
              <li key={ev.id} className="flex flex-wrap items-center gap-3 py-2 text-sm">
                <span className="font-mono text-xs">{ev.version}</span>
                {ev.previous_version && <span className="text-xs text-faint">from {ev.previous_version}</span>}
                {ev.commit && <span className="font-mono text-xs text-faint">{ev.commit.slice(0, 7)}</span>}
                <span className="ml-auto text-xs text-muted tnum" title={exact(ev.deployed_at)}>
                  {relTime(ev.deployed_at)}
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>

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
