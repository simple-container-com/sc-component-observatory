// Wire types. These mirror the Go wire structs in
// service/internal/api — NOT the stored records. The Go side emits an
// explicit wire type precisely so this file can be a faithful mirror
// of one place, and every array it declares is non-nullable there.

export type Health = "up" | "down" | "unknown";

export interface TelemetrySource {
  kind: string;
  ref: string;
  region: string;
  dims: Record<string, string>;
}

export interface Service {
  stack: string;
  service: string;
  env: string;
  cloud: string;
  template: string;
  version: string;
  commit: string;
  short_commit: string;
  /**
   * Where `version` came from. The two can disagree, and that
   * disagreement is the interesting case: it means a deploy did not
   * replace the running code.
   *
   * "deploy" — recorded at registration; what SC deployed.
   * "probe"  — read from the service's own health response; what is
   *            actually running. This is the common case today,
   *            because the plugin bus carries no version field.
   * ""       — unknown.
   */
  version_source?: "deploy" | "probe" | "";
  /** Always the probe-reported value, when there is one. */
  live_version?: string;
  /**
   * `<owner>/<repo>`, when the plugin bus carried it (api#640).
   * Empty against an older SC core, so every use must be conditional.
   */
  repository?: string;
  deployed_at: string;
  first_seen_at: string;
  last_seen_at: string;
  endpoint_url: string;
  /** Always an array, never null — the Go side guarantees it. */
  sources: TelemetrySource[];
  /** Telemetry handles were guessed from naming, not published. */
  inferred: boolean;
  stale: boolean;
  telemetry_ready: boolean;
  health: Health;
  health_at: string;
  health_path: string;
  regions: string[];
  has_client_config: boolean;
}

/** One leaf of the client config that differs between two deploys. */
export interface ConfigChange {
  /** Full dotted path into the snapshot — two buckets can share a key. */
  path: string;
  /** Display label within its bucket. */
  key: string;
  kind: "added" | "removed" | "changed";
  previous?: string;
  current?: string;
  /**
   * The value was redacted at the source. Both sides render as
   * `[redacted]`: the plugin's keyed fingerprints already proved they
   * differ, which is the entire signal.
   */
  secret?: boolean;
}

/**
 * What changed between a deploy and the one before it.
 *
 * `unavailable` is load-bearing and must be rendered whenever set: an
 * absent snapshot is NOT "nothing changed", and conflating the two is
 * the silent degradation this component exists to avoid. All four
 * buckets are always arrays — the Go side guarantees it.
 */
export interface ConfigDiff {
  env: ConfigChange[];
  compute: ConfigChange[];
  settings: ConfigChange[];
  artifact: ConfigChange[];
  unavailable?: string;
  truncated?: boolean;
}

export interface DeployEvent {
  id: string;
  stack: string;
  version: string;
  commit?: string;
  short_commit?: string;
  template: string;
  cloud: string;
  repository?: string;
  deployed_at: string;
  previous_version?: string;
  /**
   * A config snapshot was stored for this deploy. False for every
   * deploy that predates snapshot capture, and for one whose plugin
   * sent no client config — in both cases a diff is impossible and
   * `diff.unavailable` says which.
   */
  config_captured?: boolean;
  /** Only present on `GET /services/{stack}/deploys`. */
  diff?: ConfigDiff;
}

export interface DeploysResponse {
  stack: string;
  /** Rows on THIS page. */
  count: number;
  /**
   * Deploys the store could see, so the UI can say "20 of 63" instead
   * of implying a page is the whole history.
   */
  total: number;
  deploys: DeployEvent[];
  /** Absent when this is the oldest page. */
  next_cursor?: string;
  /**
   * The store's history scan cap was reached. Set on every page, not
   * just the last one, because the end of a capped timeline is
   * indistinguishable from the first deploy unless it is named.
   */
  capped?: boolean;
  cap_reason?: string;
}

export interface HealthResult {
  stack: string;
  region?: string;
  at: string;
  status: Health;
  http_status?: number;
  latency_ms?: number;
  error?: string;
  url?: string;
}

export interface ServicesResponse {
  count: number;
  services: Service[];
}

export interface ServiceDetail {
  service: Service;
  deploys: DeployEvent[];
  health: Record<string, HealthResult>;
  client_config: Record<string, unknown>;
}

export interface SessionInfo {
  authenticated: boolean;
  password_set: boolean;
}

/** One log line. */
export interface LogEvent {
  at: string;
  message: string;
  /** Log stream / pod / container the line came from. */
  stream?: string;
  /** The handle that produced it — a page can merge several groups. */
  source?: string;
  id?: string;
  /** Cut to the per-message byte cap; the rest of the line is not here. */
  truncated?: boolean;
}

/**
 * One page of logs, plus everything needed to say honestly what was and
 * was not read.
 *
 * Four fields are load-bearing and must always be rendered when set,
 * for the same reason `ConfigDiff.unavailable` is:
 *
 * - `unavailable` — there was nothing to query, or nothing could be
 *   queried. NOT "the service was quiet". When set, `events` is empty
 *   by construction.
 * - `capped` + `cap_reason` — a budget stopped the scan, so these are
 *   the newest events of the part of the window that was scanned, not
 *   of the window. A capped page that renders like a complete one is
 *   the failure this component exists to prevent.
 * - `warnings` — per-source failures that did not sink the query (one
 *   log group denied out of three). Dropping them would present a
 *   partial answer as complete.
 * - `inferred` — the handles were guessed from naming conventions, so
 *   an empty page may be a wrong lookup rather than a quiet service.
 *
 * `events`, `queried` and `warnings` are always arrays — the Go side
 * guarantees it.
 */
export interface LogsResponse {
  stack: string;
  /** The cloud implementation that served this, so its limits apply. */
  provider?: string;
  /** The window ACTUALLY searched. Echoed so the UI labels the truth. */
  window_start: string;
  window_end: string;
  /** Newest-first. */
  events: LogEvent[];
  /** Continues OLDER than this page. Absent means the window is done. */
  next_cursor?: string;
  capped: boolean;
  cap_reason?: string;
  /** What the query cost, surfaced to whoever triggered the scan. */
  scanned: number;
  calls: number;
  /** The concrete handles read — the first question when a search is empty. */
  queried: string[];
  warnings: string[];
  unavailable?: string;
  inferred: boolean;
  filter?: string;
}

/**
 * Log query parameters.
 *
 * `cursor` moves only the window's END, so a caller paging backwards
 * MUST resend the same window — hence `start`/`end` alongside it rather
 * than `minutes`, which is relative to now and would drift the window
 * under the reader between pages.
 */
export interface LogsQuery {
  q?: string;
  minutes?: number;
  start?: string;
  end?: string;
  limit?: number;
  cursor?: string;
}
