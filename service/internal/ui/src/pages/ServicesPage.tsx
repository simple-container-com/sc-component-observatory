import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { ChevronRight, RefreshCw } from "lucide-react";
import { api, describeError } from "@/api/client";
import { Empty, ErrorBox, HealthBadge, Pill, Spinner, exact, relTime } from "@/components/bits";

export function ServicesPage() {
  const q = useQuery({
    queryKey: ["services"],
    queryFn: api.services,
    // The registry only changes on a deploy or a healthcheck tick, so
    // polling hard buys nothing. 30s keeps health reasonably fresh.
    refetchInterval: 30_000,
  });

  if (q.isLoading) return <Spinner label="Loading services…" />;
  if (q.error) return <ErrorBox message={describeError(q.error)} onRetry={() => void q.refetch()} />;

  const services = q.data?.services ?? [];
  if (!services.length) {
    return (
      <Empty title="No services registered yet">
        A service registers itself on its next deploy. Add{" "}
        <code className="font-mono text-xs">my-observatory</code> to its{" "}
        <code className="font-mono text-xs">uses:</code> list — it must be last.
      </Empty>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-muted">
          {services.length} service{services.length === 1 ? "" : "s"}
        </p>
        <button className="btn inline-flex items-center gap-1.5" onClick={() => void q.refetch()} disabled={q.isFetching}>
          <RefreshCw className={q.isFetching ? "h-3.5 w-3.5 animate-spin" : "h-3.5 w-3.5"} />
          Refresh
        </button>
      </div>

      <div className="card overflow-hidden">
        <div className="hidden grid-cols-[minmax(0,1fr)_90px_110px_130px_100px_20px] gap-3 border-b border-line px-4 py-2 text-xs text-muted md:grid">
          <span>Service</span>
          <span>Health</span>
          <span>Version</span>
          <span>Deployed</span>
          <span>Target</span>
          <span />
        </div>
        <ul className="divide-y divide-line">
          {services.map((s) => (
            <li key={s.stack}>
              <Link
                to={`/services/${encodeURIComponent(s.stack)}`}
                className="grid grid-cols-1 items-center gap-2 px-4 py-3 hover:bg-bg/60 md:grid-cols-[minmax(0,1fr)_90px_110px_130px_100px_20px] md:gap-3"
              >
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <span className="truncate font-medium">{s.service || s.stack}</span>
                    {s.env && <Pill>{s.env}</Pill>}
                    {s.stale && <Pill tone="warn" title="No deploy registered recently — this service may be gone">stale</Pill>}
                  </div>
                  {s.endpoint_url && <p className="truncate text-xs text-faint">{s.endpoint_url}</p>}
                </div>

                <HealthBadge status={s.health} title={s.health_at ? `Last probe ${exact(s.health_at)}` : "Never probed"} />

                <span className="truncate font-mono text-xs" title={s.version}>
                  {s.version || "—"}
                  {s.short_commit && <span className="ml-1 text-faint">{s.short_commit}</span>}
                </span>

                <span className="text-xs text-muted tnum" title={exact(s.deployed_at)}>
                  {relTime(s.deployed_at)}
                </span>

                <span className="flex flex-wrap gap-1">
                  {s.cloud && <Pill>{s.cloud}</Pill>}
                  {s.template && <Pill>{s.template}</Pill>}
                </span>

                <ChevronRight className="hidden h-4 w-4 text-faint md:block" />
              </Link>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}
