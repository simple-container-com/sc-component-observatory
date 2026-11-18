import clsx from "clsx";
import type { Health } from "@/api/types";

/** Relative time, falling back to the raw string if unparseable. */
export function relTime(iso: string): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const secs = Math.round((Date.now() - t) / 1000);
  if (secs < 60) return "just now";
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 48) return `${hrs}h ago`;
  return `${Math.round(hrs / 24)}d ago`;
}

export function exact(iso: string): string {
  if (!iso) return "";
  const t = Date.parse(iso);
  return Number.isNaN(t) ? iso : new Date(t).toISOString().replace("T", " ").slice(0, 19) + "Z";
}

const HEALTH_TONE: Record<Health, string> = {
  up: "bg-ok/15 text-ok border-ok/30",
  down: "bg-danger/15 text-danger border-danger/30",
  unknown: "bg-faint/10 text-faint border-faint/30",
};

const HEALTH_LABEL: Record<Health, string> = {
  up: "Up",
  down: "Down",
  // "Unknown" is a distinct state on purpose: it means we have not
  // probed, or the service published no endpoint. Showing it as down
  // would invent an outage; showing it as up would hide a wiring gap.
  unknown: "Unknown",
};

export function HealthBadge({ status, title }: { status: Health; title?: string }) {
  return (
    <span
      title={title}
      className={clsx("inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium", HEALTH_TONE[status] ?? HEALTH_TONE.unknown)}
    >
      {HEALTH_LABEL[status] ?? status}
    </span>
  );
}

export function Pill({ children, tone = "neutral", title }: { children: React.ReactNode; tone?: "neutral" | "warn" | "accent"; title?: string }) {
  return (
    <span
      title={title}
      className={clsx(
        "inline-flex items-center rounded-md border px-1.5 py-0.5 text-[11px]",
        tone === "warn" && "border-warn/40 bg-warn/10 text-warn",
        tone === "accent" && "border-accent/40 bg-accent/10 text-accent",
        tone === "neutral" && "border-line bg-bg text-muted",
      )}
    >
      {children}
    </span>
  );
}

export function Empty({ title, children }: { title: string; children?: React.ReactNode }) {
  return (
    <div className="card p-8 text-center">
      <p className="font-medium">{title}</p>
      {children && <p className="mt-1 text-sm text-muted">{children}</p>}
    </div>
  );
}

export function ErrorBox({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div role="alert" className="card border-danger/30 bg-danger/5 p-4">
      <p className="text-sm font-medium text-danger">Something went wrong</p>
      <p className="mt-1 break-words font-mono text-xs text-muted">{message}</p>
      {onRetry && (
        <button type="button" className="btn mt-3" onClick={onRetry}>
          Retry
        </button>
      )}
    </div>
  );
}

export function Spinner({ label = "Loading…" }: { label?: string }) {
  return <p className="p-8 text-center text-sm text-muted">{label}</p>;
}
