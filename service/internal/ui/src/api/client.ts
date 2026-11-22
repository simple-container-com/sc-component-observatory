import type {
  DeploysResponse,
  LogsQuery,
  LogsResponse,
  ServiceDetail,
  ServicesResponse,
  SessionInfo,
} from "./types";

/**
 * Every error path on the Go side returns parseable JSON with an
 * `error` key — deliberately, because the first SC Component served
 * HTML 404s to a JSON-expecting SPA and the resulting crash was
 * unreadable. So this can always try JSON first and still degrade
 * safely if something upstream (a proxy, a 502 page) does not.
 */
export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) {
    let msg = text || `HTTP ${res.status}`;
    try {
      const parsed = JSON.parse(text);
      if (parsed?.error) msg = parsed.error;
    } catch {
      // Not JSON — keep the raw text, truncated so a stray HTML page
      // cannot flood the UI.
      msg = text.slice(0, 200) || `HTTP ${res.status}`;
    }
    throw new ApiError(res.status, msg);
  }
  return (text ? JSON.parse(text) : null) as T;
}

export const api = {
  session: () => request<SessionInfo>("GET", "/api/v1/session"),
  login: (password: string) => request<null>("POST", "/api/v1/login", { password }),
  logout: () => request<null>("POST", "/api/v1/logout"),
  services: () => request<ServicesResponse>("GET", "/api/v1/services"),
  service: (stack: string) =>
    request<ServiceDetail>("GET", `/api/v1/services/${encodeURIComponent(stack)}`),
  /**
   * The deploy timeline WITH a per-deploy config diff. Separate from
   * `service()` because each row costs the backend a snapshot read, so
   * the detail payload carries the timeline without diffs and this is
   * fetched alongside it.
   */
  deploys: (stack: string, limit?: number) =>
    request<DeploysResponse>(
      "GET",
      `/api/v1/services/${encodeURIComponent(stack)}/deploys${limit ? `?limit=${limit}` : ""}`,
    ),
  /**
   * One page of logs, newest-first.
   *
   * Never called on a timer and never on a keystroke: every call is a
   * real CloudWatch scan with a real bill, so the panel fetches on an
   * explicit submit and on "load older" only.
   */
  logs: (stack: string, query: LogsQuery = {}) => {
    const p = new URLSearchParams();
    if (query.q) p.set("q", query.q);
    if (query.minutes) p.set("minutes", String(query.minutes));
    if (query.start) p.set("start", query.start);
    if (query.end) p.set("end", query.end);
    if (query.limit) p.set("limit", String(query.limit));
    if (query.cursor) p.set("cursor", query.cursor);
    const qs = p.toString();
    return request<LogsResponse>(
      "GET",
      `/api/v1/services/${encodeURIComponent(stack)}/logs${qs ? `?${qs}` : ""}`,
    );
  },
  removeService: (stack: string) =>
    request<{ deleted: string }>("DELETE", `/api/v1/services/${encodeURIComponent(stack)}`),
};

export function describeError(e: unknown): string {
  if (e instanceof ApiError) return e.message;
  if (e instanceof Error) return e.message;
  return String(e);
}
