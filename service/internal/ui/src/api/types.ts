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

export interface DeployEvent {
  id: string;
  stack: string;
  version: string;
  commit?: string;
  template: string;
  cloud: string;
  deployed_at: string;
  previous_version?: string;
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
