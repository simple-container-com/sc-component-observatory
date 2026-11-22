import { useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, ChevronRight, ExternalLink, RefreshCw, Search } from "lucide-react";
import { api, describeError } from "@/api/client";
import type { ConfigChange, ConfigDiff, DeployEvent, MetricSeries } from "@/api/types";
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

      <LogsPanel
        stack={s.stack}
        inferred={s.inferred}
        handles={s.sources.filter((x) => x.kind.includes("log")).map((x) => x.ref)}
      />

      <MetricsPanel
        stack={s.stack}
        inferred={s.inferred}
        /* Cosmetic only — used for the "nothing to query" message and
           until a query has run. The BACKEND decides which handles it
           reads (by source kind), and its `queried` list replaces this
           one as soon as there is a response, so the SPA never has to
           stay in step with the registry's kind vocabulary. */
        handles={s.sources.filter((x) => x.kind.includes("metric")).map((x) => x.ref)}
      />

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

/** Rows per timeline page. Matches the backend's own default. */
const DEPLOY_PAGE = 20;

/**
 * Open/closed state for a collapsible card, persisted per key.
 *
 * Persisted because the alternative is worse than it sounds: an
 * operator comparing two services navigates between detail pages
 * repeatedly, and a card that springs back open every time is a card
 * they collapse every time. localStorage access is wrapped — a blocked
 * store (private mode, a locked-down browser) must degrade to a working
 * default, never throw inside a render.
 */
function useCollapsed(key: string, defaultOpen: boolean) {
  const [open, setOpen] = useState(() => {
    try {
      const v = localStorage.getItem(key);
      return v === null ? defaultOpen : v === "1";
    } catch {
      return defaultOpen;
    }
  });
  return [
    open,
    (next: boolean) => {
      setOpen(next);
      try {
        localStorage.setItem(key, next ? "1" : "0");
      } catch {
        /* nothing to do; the choice just won't outlive the page */
      }
    },
  ] as const;
}

/**
 * Deploy timeline with, per row, what changed since the deploy before
 * it. Collapsible, and paged.
 *
 * Its own query: the diff costs the backend TWO snapshot reads per row
 * (one memoized away within a page), so the detail payload carries the
 * timeline WITHOUT diffs and this fetches the annotated version
 * alongside it. `fallback` is that diff-less list — if the diff query
 * fails, the timeline still renders and the failure is reported in
 * place of the diffs, rather than the whole card disappearing over an
 * additive feature.
 *
 * Collapsing DISABLES the query rather than just hiding its output.
 * Same discipline as the logs panel: a card whose every page costs real
 * object reads should not be paying for them while rolled up. The
 * consequence is deliberate — a collapsed header counts deploys from
 * the free `fallback` list, so the number can lag the paged `total` by
 * whatever the detail payload's own cap (50) hides.
 */
function DeployTimeline({ stack, fallback }: { stack: string; fallback: DeployEvent[] }) {
  const [open, setOpen] = useCollapsed("obs.deploys.open", true);

  const q = useInfiniteQuery({
    queryKey: ["deploys", stack],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => api.deploys(stack, { limit: DEPLOY_PAGE, cursor: pageParam }),
    /* The cursor is opaque and comes straight from the server. Pages
       are stable boundaries, not offsets, so a deploy landing between
       two page fetches cannot shift what the next page returns. */
    getNextPageParam: (last) => last.next_cursor ?? undefined,
    enabled: open,
    refetchInterval: open ? 60_000 : false,
    refetchOnWindowFocus: false,
  });

  const pages = q.data?.pages ?? [];
  const paged = pages.flatMap((p) => p.deploys);
  const total = pages[0]?.total;
  const capped = pages.find((p) => p.capped);

  // The diff query failed and there is nothing paged: fall back to the
  // diff-less list from the detail payload rather than showing an empty
  // timeline for a service that has deploys.
  const usingFallback = paged.length === 0 && !!q.error;
  const rows = usingFallback ? fallback : paged;

  const count = open && total !== undefined ? total : fallback.length;

  return (
    <div className="card p-4">
      <button
        type="button"
        className="flex w-full items-center gap-2 text-left"
        onClick={() => setOpen(!open)}
        aria-expanded={open}
      >
        <ChevronRight className={"h-3.5 w-3.5 shrink-0 text-faint transition-transform " + (open ? "rotate-90" : "")} />
        <h2 className="text-sm font-medium">Deploy history</h2>
        {count > 0 && (
          <span className="text-xs text-faint tnum">
            {count} deploy{count === 1 ? "" : "s"}
          </span>
        )}
        {/* Collapsed, the latest version is the one fact worth keeping
            on screen — it is what the rest of the page is about. */}
        {!open && fallback[0] && <span className="font-mono text-xs text-faint">latest {fallback[0].version}</span>}
        {open && q.isFetching && !q.data && <span className="text-xs text-faint">loading diffs…</span>}
      </button>

      {!open ? null : (
        <>
          {q.error && (
            <p className="mt-2 text-xs text-warn">
              Diffs unavailable: {describeError(q.error)}
              {usingFallback && " — showing the timeline without them"}
            </p>
          )}

          {capped?.cap_reason && (
            <p className="mt-3 rounded border border-warn/40 bg-warn/5 p-2 text-xs text-warn">{capped.cap_reason}</p>
          )}

          {q.isLoading ? (
            <Spinner label="Reading deploy history…" />
          ) : rows.length === 0 ? (
            <p className="mt-2 text-sm text-muted">No deploys recorded yet.</p>
          ) : (
            <ul className="mt-3 divide-y divide-line/60">
              {rows.map((ev) => (
                <DeployRow key={ev.id} ev={ev} />
              ))}
            </ul>
          )}

          {q.hasNextPage ? (
            <button
              type="button"
              className="btn mt-3 w-full"
              onClick={() => void q.fetchNextPage()}
              disabled={q.isFetchingNextPage}
            >
              {q.isFetchingNextPage
                ? "Loading…"
                : `Load older deploys${total ? ` — ${rows.length} of ${total} shown` : ""}`}
            </button>
          ) : usingFallback ? (
            /* NOT "the whole history": the fallback list is the detail
               payload's own newest-50, and saying otherwise would turn
               a degraded read into a false claim about the service. */
            <p className="mt-3 text-xs text-faint">Up to the newest 50 deploys, without diffs.</p>
          ) : rows.length > 0 && !capped ? (
            <p className="mt-3 text-xs text-faint">
              {rows.length === 1 ? "The only recorded deploy." : `All ${rows.length} recorded deploys.`}
            </p>
          ) : null}
        </>
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

const WINDOWS: { label: string; minutes: number }[] = [
  { label: "15m", minutes: 15 },
  { label: "1h", minutes: 60 },
  { label: "6h", minutes: 360 },
  { label: "24h", minutes: 1440 },
];

const PAGE = 200;

/**
 * Logs panel.
 *
 * Deliberately NOT on a refetch interval, unlike every other query on
 * this page: each fetch is a real CloudWatch scan with a real bill, and
 * a dashboard left open on a wall would run one every interval forever.
 * It fetches on mount (bounded by the short default window), then only
 * on an explicit search, window change or "load older".
 *
 * Search is submit-driven for the same reason — a scan per keystroke is
 * a scan per keystroke.
 */
function LogsPanel({ stack, inferred, handles }: { stack: string; inferred: boolean; handles: string[] }) {
  const [minutes, setMinutes] = useState(15);
  const [draft, setDraft] = useState("");
  const [filter, setFilter] = useState("");

  const q = useInfiniteQuery({
    queryKey: ["logs", stack, minutes, filter],
    initialPageParam: null as { cursor: string; start: string; end: string } | null,
    queryFn: ({ pageParam }) =>
      api.logs(
        stack,
        pageParam
          ? { q: filter, limit: PAGE, cursor: pageParam.cursor, start: pageParam.start, end: pageParam.end }
          : { q: filter, limit: PAGE, minutes },
      ),
    /* Pages 2..n resend the FIRST page's window verbatim. The cursor
       moves only the window's end, so using `minutes` again would slide
       the window forward under the reader between pages and quietly
       skip whatever was written in between. */
    getNextPageParam: (last, all) =>
      last.next_cursor
        ? { cursor: last.next_cursor, start: all[0].window_start, end: all[0].window_end }
        : undefined,
    enabled: handles.length > 0,
    refetchOnWindowFocus: false,
  });

  const pages = q.data?.pages ?? [];
  const first = pages[0];
  const events = pages.flatMap((p) => p.events);
  const warnings = Array.from(new Set(pages.flatMap((p) => p.warnings)));
  const capped = pages.find((p) => p.capped);
  const scanned = pages.reduce((n, p) => n + p.scanned, 0);
  const calls = pages.reduce((n, p) => n + p.calls, 0);

  const header = (
    <div className="flex flex-wrap items-center gap-2">
      <h2 className="text-sm font-medium">Logs</h2>
      {inferred && (
        <Pill tone="warn" title="These handles were guessed from naming conventions, not published by the plugin that owns the resource — so an empty result may be a wrong lookup rather than a quiet service">
          inferred
        </Pill>
      )}
      {first?.provider && <span className="text-xs text-faint">{first.provider}</span>}
    </div>
  );

  // No handles at all: there is nothing to query, and saying so is
  // different from showing an empty log view.
  if (handles.length === 0) {
    return (
      <div className="card p-4">
        {header}
        <p className="mt-2 text-sm text-muted">
          No log handles are recorded for this service — its template published none and none could be inferred, so
          there is nothing to query.
        </p>
      </div>
    );
  }

  return (
    <div className="card p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        {header}
        <div className="flex items-center gap-1">
          {WINDOWS.map((w) => (
            <button
              key={w.minutes}
              type="button"
              onClick={() => setMinutes(w.minutes)}
              className={
                "rounded px-2 py-1 text-xs " +
                (w.minutes === minutes ? "bg-accent/15 text-accent" : "text-muted hover:text-fg")
              }
            >
              {w.label}
            </button>
          ))}
          <button
            type="button"
            className="btn ml-1 px-2 py-1"
            onClick={() => void q.refetch()}
            disabled={q.isFetching}
            title="Re-run the scan. Not automatic: each run is a real CloudWatch scan."
          >
            <RefreshCw className={"h-3.5 w-3.5 " + (q.isFetching ? "animate-spin" : "")} />
          </button>
        </div>
      </div>

      <form
        className="mt-3 flex gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setFilter(draft.trim());
        }}
      >
        <input
          className="input font-mono text-xs"
          placeholder="Filter pattern — CloudWatch syntax, e.g. ERROR or ?WARN ?ERROR"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
        />
        <button type="submit" className="btn whitespace-nowrap" disabled={q.isFetching}>
          <Search className="mr-1 inline h-3.5 w-3.5" />
          Search
        </button>
      </form>

      {/* The window the SERVER searched, not the one this component
          asked for. They can differ, and the label must be the truth. */}
      {first && (
        <p className="mt-2 text-xs text-faint tnum">
          {exact(first.window_start)} → {exact(first.window_end)} · scanned {scanned.toLocaleString()} event
          {scanned === 1 ? "" : "s"} in {calls} call{calls === 1 ? "" : "s"}
          {first.filter ? ` · filter ${first.filter}` : ""}
        </p>
      )}

      {q.error && <div className="mt-3"><ErrorBox message={describeError(q.error)} onRetry={() => void q.refetch()} /></div>}

      {/* Load-bearing: "we could not look" must never render as an
          empty list. */}
      {first?.unavailable && (
        <p className="mt-3 rounded border border-warn/40 bg-warn/5 p-2 text-xs text-warn">{first.unavailable}</p>
      )}

      {capped?.cap_reason && (
        <p className="mt-3 rounded border border-warn/40 bg-warn/5 p-2 text-xs text-warn">{capped.cap_reason}</p>
      )}

      {warnings.length > 0 && (
        <ul className="mt-3 space-y-1">
          {warnings.map((wn) => (
            <li key={wn} className="text-xs text-warn">
              {wn}
            </li>
          ))}
        </ul>
      )}

      {q.isLoading ? (
        <div className="mt-3"><Spinner label="Scanning…" /></div>
      ) : events.length === 0 && !first?.unavailable ? (
        <p className="mt-3 text-sm text-muted">
          No log lines in this window{first?.filter ? ` matching ${first.filter}` : ""}. The groups searched are listed
          below — if that is not where this service writes, the handles are wrong.
        </p>
      ) : (
        <ul className="mt-3 max-h-[28rem] divide-y divide-line/40 overflow-auto rounded bg-bg">
          {events.map((e, i) => (
            <li key={e.id || `${e.at}-${i}`} className="flex gap-3 px-3 py-1.5 font-mono text-[11px] leading-relaxed">
              <span className="shrink-0 text-faint tnum" title={exact(e.at)}>
                {logTime(e.at)}
              </span>
              <span className="min-w-0 whitespace-pre-wrap break-words text-muted" title={e.stream || e.source}>
                {e.message}
                {e.truncated && (
                  <span className="ml-1.5 font-sans text-[10px] text-warn" title="Cut at the per-message size cap; the rest of this line is not here">
                    truncated
                  </span>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}

      {q.hasNextPage ? (
        <button
          type="button"
          className="btn mt-3 w-full"
          onClick={() => void q.fetchNextPage()}
          disabled={q.isFetchingNextPage}
        >
          {q.isFetchingNextPage ? "Loading…" : "Load older"}
        </button>
      ) : events.length > 0 ? (
        <p className="mt-3 text-xs text-faint">Reached the start of the window.</p>
      ) : null}

      {/* "Which groups did you search" is the first question when a
          search comes back empty, so the answer is always one click
          away rather than only in a server log. */}
      <details className="mt-3">
        <summary className="cursor-pointer text-xs text-faint">
          {first?.queried.length ? `${first.queried.length} log group(s) searched` : `${handles.length} handle(s) recorded`}
        </summary>
        <ul className="mt-1 space-y-1">
          {(first?.queried.length ? first.queried : handles).map((g) => (
            <li key={g} className="truncate font-mono text-[11px] text-faint" title={g}>
              {g}
            </li>
          ))}
        </ul>
      </details>
    </div>
  );
}

/** UTC wall clock to the millisecond — log lines are ordered by it. */
function logTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toISOString().slice(11, 23) + "Z";
}

/**
 * Metric windows. Wider than the log windows on purpose: a metric read
 * costs a fixed handful of API calls whatever the window, so the reason
 * to keep log windows short (scan volume) does not apply, and three
 * hours is the narrowest window in which a 1-minute series has a shape.
 *
 * 30d is the backend's default ceiling. The provider will RAISE the
 * period past 15 days (CloudWatch stops keeping 1-minute resolution),
 * and the panel reports that rather than silently drawing a coarser
 * line.
 */
const METRIC_WINDOWS: { label: string; minutes: number }[] = [
  { label: "1h", minutes: 60 },
  { label: "3h", minutes: 180 },
  { label: "24h", minutes: 1440 },
  { label: "7d", minutes: 7 * 1440 },
  { label: "30d", minutes: 30 * 1440 },
];

/**
 * Metrics panel.
 *
 * Renders whatever series the backend hands it — no metric name, unit or
 * statistic is known here. That is the whole point: the catalogue lives
 * in the provider (`internal/telemetry/awsprov/metrics.go`), so adding
 * ECS, YC or k8s metrics is a table on the Go side and no change in this
 * file.
 *
 * Collapsible and query-disabled while collapsed, like the deploy
 * timeline — each expansion is a real CloudWatch read. Not on a refetch
 * interval, like the logs panel, for the same reason.
 */
function MetricsPanel({ stack, inferred, handles }: { stack: string; inferred: boolean; handles: string[] }) {
  const [open, setOpen] = useCollapsed("obs.metrics.open", true);
  const [minutes, setMinutes] = useState(180);

  const q = useQuery({
    queryKey: ["metrics", stack, minutes],
    queryFn: () => api.metrics(stack, { minutes }),
    enabled: open && handles.length > 0,
    refetchOnWindowFocus: false,
  });

  const res = q.data;
  const drawn = res?.series.filter((s) => !s.absent) ?? [];
  const absent = res?.series.filter((s) => s.absent) ?? [];

  const header = (
    <>
      <ChevronRight className={"h-3.5 w-3.5 shrink-0 text-faint transition-transform " + (open ? "rotate-90" : "")} />
      <h2 className="text-sm font-medium">Metrics</h2>
      {inferred && (
        <Pill
          tone="warn"
          title="These handles were guessed from naming conventions, not published by the plugin that owns the resource — so a metric that resolves to nothing may be a wrong lookup rather than an idle workload"
        >
          inferred
        </Pill>
      )}
      {res?.provider && <span className="text-xs text-faint">{res.provider}</span>}
      {open && q.isFetching && <span className="text-xs text-faint">reading…</span>}
    </>
  );

  // Nothing to query. Said in words, because an empty chart grid reads
  // as "no traffic" — the exact confusion this component exists to
  // prevent.
  if (handles.length === 0) {
    return (
      <div className="card p-4">
        <div className="flex items-center gap-2">{header}</div>
        <p className="mt-2 text-sm text-muted">
          No metric handles are recorded for this service — its template published none and none could be inferred, so
          there is nothing to query.
        </p>
      </div>
    );
  }

  return (
    <div className="card p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <button
          type="button"
          className="flex flex-1 items-center gap-2 text-left"
          onClick={() => setOpen(!open)}
          aria-expanded={open}
        >
          {header}
        </button>
        {open && (
          <div className="flex items-center gap-1">
            {METRIC_WINDOWS.map((w) => (
              <button
                key={w.minutes}
                type="button"
                onClick={() => setMinutes(w.minutes)}
                className={
                  "rounded px-2 py-1 text-xs " +
                  (w.minutes === minutes ? "bg-accent/15 text-accent" : "text-muted hover:text-fg")
                }
              >
                {w.label}
              </button>
            ))}
            <button
              type="button"
              className="btn ml-1 px-2 py-1"
              onClick={() => void q.refetch()}
              disabled={q.isFetching}
              title="Re-read. Not automatic: each read is a real CloudWatch query."
            >
              <RefreshCw className={"h-3.5 w-3.5 " + (q.isFetching ? "animate-spin" : "")} />
            </button>
          </div>
        )}
      </div>

      {!open ? null : (
        <>
          {/* The window and bucket the SERVER used, not the ones this
              panel asked for. They differ whenever the period was
              raised, and the label must be the truth. */}
          {res && (
            <p className="mt-2 text-xs text-faint tnum">
              {exact(res.window_start)} → {exact(res.window_end)} · {describePeriod(res.period_seconds)} ·{" "}
              {res.queried.length} target{res.queried.length === 1 ? "" : "s"} in {res.calls} call
              {res.calls === 1 ? "" : "s"}
            </p>
          )}

          {q.error && (
            <div className="mt-3">
              <ErrorBox message={describeError(q.error)} onRetry={() => void q.refetch()} />
            </div>
          )}

          {/* Load-bearing: "we could not look" must never render as a
              set of empty charts. */}
          {res?.unavailable && (
            <p className="mt-3 rounded border border-warn/40 bg-warn/5 p-2 text-xs text-warn">{res.unavailable}</p>
          )}

          {/* A raised period is not a failure, but it IS a different
              chart from the one that was asked for: 5-minute buckets
              flatten a spike a 1-minute series would show. */}
          {res?.adjusted && res.adjust_reason && (
            <p className="mt-3 rounded border border-line bg-bg p-2 text-xs text-muted">{res.adjust_reason}</p>
          )}

          {res?.capped && res.cap_reason && (
            <p className="mt-3 rounded border border-warn/40 bg-warn/5 p-2 text-xs text-warn">{res.cap_reason}</p>
          )}

          {res && res.warnings.length > 0 && (
            <ul className="mt-3 space-y-1">
              {res.warnings.map((wn) => (
                <li key={wn} className="text-xs text-warn">
                  {wn}
                </li>
              ))}
            </ul>
          )}

          {q.isLoading ? (
            <div className="mt-3">
              <Spinner label="Reading metrics…" />
            </div>
          ) : !res ? null : (
            <>
              {drawn.length > 0 && (
                <div className="mt-3 grid gap-3 md:grid-cols-2">
                  {drawn.map((s) => (
                    <MetricChart
                      key={`${s.name}|${s.label ?? ""}`}
                      series={s}
                      start={res.window_start}
                      end={res.window_end}
                      periodSeconds={res.period_seconds}
                    />
                  ))}
                </div>
              )}

              {/* An absent metric is listed, never omitted and never
                  drawn as a flat line at zero: "this workload has no
                  such metric" and "it reported nothing" are different
                  answers, and `ListMetrics` is called to tell them
                  apart. */}
              {absent.length > 0 && (
                <ul className="mt-3 space-y-1">
                  {absent.map((s) => (
                    <li key={`${s.name}|${s.label ?? ""}`} className="text-xs text-faint">
                      <span className="font-mono text-muted">{s.label || s.name}</span> — {s.reason || "not available"}
                    </li>
                  ))}
                </ul>
              )}

              {drawn.length === 0 && absent.length === 0 && !res.unavailable && (
                <p className="mt-3 text-sm text-muted">
                  No series came back for this window. The targets queried are listed below — if that is not this
                  service's workload, the handles are wrong.
                </p>
              )}
            </>
          )}

          {/* "What did you actually chart" is the first question when a
              chart looks wrong, so the answer is one click away. */}
          <details className="mt-3">
            <summary className="cursor-pointer text-xs text-faint">
              {res?.queried.length
                ? `${res.queried.length} target(s) charted`
                : `${handles.length} handle(s) recorded`}
            </summary>
            <ul className="mt-1 space-y-1">
              {(res?.queried.length ? res.queried : handles).map((t) => (
                <li key={t} className="truncate font-mono text-[11px] text-faint" title={t}>
                  {t}
                </li>
              ))}
            </ul>
          </details>
        </>
      )}
    </div>
  );
}

/** Chart geometry. A viewBox, scaled by CSS — no measuring, no resize observer. */
const CHART_W = 600;
const CHART_H = 120;

/**
 * One series as an area chart.
 *
 * Hand-rolled SVG rather than a charting library: the whole panel is
 * five small sparklines whose only hard requirement is that they do not
 * lie, and a dependency would be an architectural decision for a
 * 40-line path builder.
 *
 * Two honesty rules are built into the geometry:
 *
 *  - X is TIME, not point index. Buckets with no data are omitted by
 *    CloudWatch, so index-positioning would compress a gap away and
 *    draw a continuous line over a period of silence.
 *  - A gap wider than two buckets BREAKS the line instead of
 *    interpolating across it. An interpolated segment over missing data
 *    is indistinguishable from real data, which is the same failure as
 *    an empty chart reading as zero traffic.
 */
function MetricChart({
  series,
  start,
  end,
  periodSeconds,
}: {
  series: MetricSeries;
  start: string;
  end: string;
  periodSeconds: number;
}) {
  const t0 = Date.parse(start);
  const t1 = Date.parse(end);
  const pts = series.points
    .map((p) => ({ t: Date.parse(p.at), v: p.value }))
    .filter((p) => Number.isFinite(p.t) && Number.isFinite(p.v))
    .sort((a, b) => a.t - b.t);

  const values = pts.map((p) => p.v);
  const peak = values.length ? Math.max(...values) : 0;
  const total = values.reduce((a, b) => a + b, 0);
  const last = values.length ? values[values.length - 1] : undefined;

  // Zero-based on purpose: for a counter a non-zero baseline invents a
  // trend, and for a latency it exaggerates noise into a cliff.
  const top = peak > 0 ? peak : 1;
  const span = Math.max(t1 - t0, 1);
  const x = (t: number) => ((t - t0) / span) * CHART_W;
  const y = (v: number) => CHART_H - (v / top) * (CHART_H - 4) - 2;

  // Break the line across gaps wider than two buckets.
  const maxGap = Math.max(periodSeconds, 60) * 2000;
  const runs: { t: number; v: number }[][] = [];
  for (const p of pts) {
    const run = runs[runs.length - 1];
    if (!run || p.t - run[run.length - 1].t > maxGap) runs.push([p]);
    else run.push(p);
  }

  return (
    <div className="rounded border border-line bg-bg p-3">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="font-mono text-xs text-fg" title={series.label || series.name}>
          {series.name}
        </span>
        {series.stat && (
          <span className="text-[10px] text-faint" title="The statistic this series was aggregated with">
            {series.stat.toLowerCase()}
          </span>
        )}
        <span className="ml-auto text-xs text-muted tnum">{summarize(series, { peak, total, last })}</span>
      </div>

      {pts.length === 0 ? (
        /* Exists, reported nothing. Said in words — a flat line at zero
           would claim a measurement that was never taken. */
        <p className="mt-2 text-xs text-faint">{series.reason || "no data points in this window"}</p>
      ) : (
        <svg
          viewBox={`0 0 ${CHART_W} ${CHART_H}`}
          preserveAspectRatio="none"
          className="mt-2 h-20 w-full"
          role="img"
          aria-label={`${series.name} over the selected window`}
        >
          {/* Mid gridline, labelled by the header's peak — enough to read
              a magnitude off the shape without a full axis. */}
          <line x1="0" y1={y(top / 2)} x2={CHART_W} y2={y(top / 2)} className="stroke-line" strokeWidth="1" />
          {runs.map((run, i) => {
            const line = run.map((p) => `${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(" L");
            const area =
              `M${x(run[0].t).toFixed(1)},${CHART_H} L` +
              line +
              ` L${x(run[run.length - 1].t).toFixed(1)},${CHART_H} Z`;
            return (
              <g key={i}>
                <path d={area} className="fill-accent/15" />
                <path
                  d={`M${line}`}
                  className="stroke-accent"
                  strokeWidth="1.5"
                  fill="none"
                  vectorEffect="non-scaling-stroke"
                />
                {/* A single-point run has no line to draw. */}
                {run.length === 1 && <circle cx={x(run[0].t)} cy={y(run[0].v)} r="2" className="fill-accent" />}
              </g>
            );
          })}
        </svg>
      )}

      <div className="mt-1 flex justify-between text-[10px] text-faint tnum">
        <span title={exact(start)}>{clock(start)}</span>
        <span>
          peak {fmtValue(peak, series.unit)}
          {/* Gaps are worth naming when a mostly-continuous line has
              holes in it. For a sparse counter — one isolated point
              per incident — every point is its own run, and counting
              those as gaps says nothing the dots do not already show. */}
          {runs.length > 1 && pts.length >= runs.length * 2
            ? ` · ${runs.length - 1} gap${runs.length === 2 ? "" : "s"}`
            : ""}
        </span>
        <span title={exact(end)}>{clock(end)}</span>
      </div>
    </div>
  );
}

/**
 * The one number worth putting next to the chart, chosen by the
 * statistic: a counter's total, a latency's peak-and-last, a ceiling's
 * peak. Driven by `stat` rather than by metric name, so it works for
 * metrics this build has never heard of.
 */
function summarize(
  s: MetricSeries,
  agg: { peak: number; total: number; last?: number },
): string {
  if (agg.last === undefined) return "—";
  switch ((s.stat || "").toLowerCase()) {
    case "sum":
      return `${fmtValue(agg.total, s.unit)} total`;
    case "maximum":
      return `${fmtValue(agg.peak, s.unit)} peak`;
    default:
      return `${fmtValue(agg.last, s.unit)} latest`;
  }
}

/** Unit-aware value formatting. Unknown units fall through to a plain number. */
function fmtValue(v: number, unit?: string): string {
  switch (unit) {
    case "Milliseconds":
      return v >= 1000 ? `${round(v / 1000)} s` : `${round(v)} ms`;
    case "Seconds":
      return v < 1 ? `${round(v * 1000)} ms` : `${round(v)} s`;
    case "Percent":
      return `${round(v)}%`;
    case "Bytes":
      return v >= 1 << 20 ? `${round(v / (1 << 20))} MiB` : v >= 1024 ? `${round(v / 1024)} KiB` : `${round(v)} B`;
    default:
      return round(v).toLocaleString();
  }
}

/** Two significant-ish digits: metric values span counts and milliseconds. */
function round(v: number): number {
  const a = Math.abs(v);
  if (a >= 100) return Math.round(v);
  if (a >= 10) return Math.round(v * 10) / 10;
  return Math.round(v * 100) / 100;
}

/** HH:MM in the reader's own zone — the axis is read against a wall clock. */
function clock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

/** "1-minute buckets" — the period the provider actually used. */
function describePeriod(seconds: number): string {
  if (!seconds) return "period unknown";
  if (seconds % 86400 === 0) return `${seconds / 86400}-day buckets`;
  if (seconds % 3600 === 0) return `${seconds / 3600}-hour buckets`;
  if (seconds % 60 === 0) return `${seconds / 60}-minute buckets`;
  return `${seconds}-second buckets`;
}
