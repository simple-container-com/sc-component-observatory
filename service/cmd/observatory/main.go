// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Command observatory is the deployed half of the sc-observatory
// Component: the service registry, the telemetry query API, and
// (from slice 5) the operator SPA.
//
// Env it reads — all of it emitted by the plugin, so an operator
// writes none of these in server.yaml:
//
//	SERVICE_JWT_SECRET               HS256 key, 32+ bytes. Shared with the
//	                                 plugin, which mints per-consumer
//	                                 registration JWTs. May arrive as a
//	                                 SecretsManager ARN (see secret_resolver.go).
//	OBSERVATORY_OPERATOR_PASSWORD    shared operator login secret; same ARN caveat.
//	ADMIN_MIGRATION_TOKEN            bearer for the one-off
//	                                 POST /admin/migrate-s3-to-goembedb; same ARN
//	                                 caveat. Absent ⇒ the route is not mounted,
//	                                 which is the steady state.
//	STORAGE_TYPE                     s3 | filesystem        (default s3)
//	STORAGE_BUCKET                   bucket for s3
//	STORAGE_REGION                   region for s3          (default eu-central-1)
//	STORAGE_PREFIX                   optional key namespace
//	STORAGE_ENDPOINT_URL             S3-compatible gateway   (optional)
//	STORAGE_USE_PATH_STYLE           non-empty ⇒ path-style  (optional)
//	STORAGE_PATH                     mount point for filesystem
//	STORAGE_ACCESS_KEY / _SECRET_KEY optional; prefer the execution role
//	METADATA_TYPE                    colocated               (default; only one in slice 1)
//	PORT                             HTTP port               (default 8080)
//
// Telemetry knobs — all optional, and all of them LOWER a limit rather
// than raise one. The defaults bound a real CloudWatch bill and a real
// Lambda timeout, so the safe direction to move them is down:
//
//	TELEMETRY_AWS_REGION             fallback region for handles that carry none
//	TELEMETRY_AWS_ACCESS_KEY/_SECRET_KEY  cross-account only; normally absent,
//	                                 because the execution role is granted
//	                                 the log-read actions directly
//	LOGS_DEFAULT_WINDOW_MINUTES      lookback when the caller names none (15)
//	LOGS_MAX_WINDOW_HOURS            widest window accepted; wider is REFUSED (48)
//	LOGS_PAGE_LIMIT / _MAX_PAGE_LIMIT  events per page (200 / 500)
//	LOGS_MAX_EVENTS_SCANNED          scan cap per request
//	LOGS_MAX_CALLS                   CloudWatch calls per request
//	LOGS_MAX_RESPONSE_KB             response-size cap
//
// Startup is fail-fast on anything that would otherwise produce a
// service that looks healthy and answers 401 or 500 to everything —
// the first SC Component spent real time on exactly that class of
// misconfiguration.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	sdkservice "github.com/simple-container-com/go-aws-lambda-sdk/pkg/service"

	"github.com/simple-container-com/sc-component-observatory/service/internal/api"
	"github.com/simple-container-com/sc-component-observatory/service/internal/health"
	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/telemetry/awsprov"
	"github.com/simple-container-com/sc-component-observatory/service/internal/ui"
)

// version is stamped at build time via -ldflags "-X main.version=...".
//
// SC injects VERSION as a build arg on EVERY image build, so a deployed
// binary reporting "0.0.0" means the Dockerfile default leaked through
// and is a bug worth chasing, not the normal reading.
var (
	version = "0.0.0"
	commit  = ""
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("startup failed", "err", err)
		os.Exit(2)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// --- secrets -----------------------------------------------------
	jwtSecret := os.Getenv("SERVICE_JWT_SECRET")
	if jwtSecret == "" {
		return errors.New("SERVICE_JWT_SECRET is required")
	}
	jwtSecret, err := resolveAWSSecretARN(ctx, "SERVICE_JWT_SECRET", jwtSecret)
	if err != nil {
		return fmt.Errorf("resolve SERVICE_JWT_SECRET: %w", err)
	}
	// HS256 floor. Enforced here rather than left to the JWT library,
	// which will happily sign with a short key.
	if len(jwtSecret) < 32 {
		return fmt.Errorf("SERVICE_JWT_SECRET must be at least 32 bytes (HS256 floor), got %d", len(jwtSecret))
	}

	operatorPassword, err := resolveAWSSecretARN(ctx, "OBSERVATORY_OPERATOR_PASSWORD", os.Getenv("OBSERVATORY_OPERATOR_PASSWORD"))
	if err != nil {
		return fmt.Errorf("resolve OBSERVATORY_OPERATOR_PASSWORD: %w", err)
	}

	// Resolved HERE, with the other two, and not at the mount site
	// below: the plugin binds this through ComponentSecretEnvMap, so on
	// a deployed Component the env var holds a SecretsManager ARN and
	// not the token. Comparing an operator's bearer against an ARN
	// string fails every request while looking perfectly configured —
	// and `ctx` carries a 30s budget from the top of run(), which an
	// embedded-go-embedb boot can eat before the mount site is reached.
	migrationToken, err := resolveAWSSecretARN(ctx, "ADMIN_MIGRATION_TOKEN", os.Getenv("ADMIN_MIGRATION_TOKEN"))
	if err != nil {
		return fmt.Errorf("resolve ADMIN_MIGRATION_TOKEN: %w", err)
	}

	// --- storage -----------------------------------------------------
	//
	// Two code paths:
	//
	//   USE_GOEMBEDB unset or "0" (DEFAULT): S3Store. Current production.
	//   USE_GOEMBEDB=1                     : MongoStore on top of an
	//                                        embedded go-embedb (ferretdb
	//                                        + sqlite + S3 replication).
	//
	// A migration endpoint copies S3 → Mongo under an admin token so an
	// operator flips the env only AFTER a dry-run shows count parity. See
	// forge/docs/roadmap/items/sc-component-observatory-goembedb-migration.md.
	//
	// migrationSource stays non-nil whenever we built an S3Store, so the
	// `/admin/migrate-s3-to-goembedb` endpoint has a source regardless of
	// which backend serves the live traffic.
	var (
		objects         storage.Store
		location        string
		migrationSource storage.Store
		migrationSink   *storage.MongoStore
	)
	useGoEmbedb := envOr("USE_GOEMBEDB", "") == "1"

	// Always build the S3 source when STORAGE_TYPE=s3 (default), both
	// as the live backend and as the migration source. A deploy with
	// USE_GOEMBEDB=1 but no S3 wiring is still valid — the migration
	// just has no source to pull from.
	if envOr("STORAGE_TYPE", "s3") == "s3" {
		s3Store, loc, serr := buildObjectStore(ctx)
		if serr != nil && !useGoEmbedb {
			return fmt.Errorf("object store: %w", serr)
		}
		if serr == nil {
			migrationSource = s3Store
			objects = s3Store
			location = loc
		}
	} else if !useGoEmbedb {
		// Non-S3 storage types (filesystem) go through the regular builder.
		st, loc, serr := buildObjectStore(ctx)
		if serr != nil {
			return fmt.Errorf("object store: %w", serr)
		}
		objects = st
		location = loc
	}

	if useGoEmbedb {
		embed, berr := bootGoEmbed(slog.Default())
		if berr != nil {
			return fmt.Errorf("goembedb boot: %w", berr)
		}
		mongoStore := storage.NewMongoStore(embed.Client)
		migrationSink = mongoStore
		objects = mongoStore
		location = "goembedb://" + embed.ReplSet
	}

	if objects == nil {
		return errors.New("no storage backend configured (USE_GOEMBEDB unset and buildObjectStore returned nil)")
	}
	api.SetStoreBackend(backendLabel(useGoEmbedb))

	// --- registry ----------------------------------------------------
	metaType := envOr("METADATA_TYPE", "colocated")
	var reg registry.Store
	switch metaType {
	case "colocated":
		reg = registry.NewColocatedStore(objects, os.Getenv("STORAGE_PREFIX"))
	default:
		// Fail loud. A typo'd METADATA_TYPE must not silently fall back
		// to a store the operator did not choose — the dashboard would
		// come up empty and look like "nothing is deployed".
		return fmt.Errorf("unsupported METADATA_TYPE %q — slice 1 ships %q only", metaType, "colocated")
	}
	defer func() { _ = reg.Close(context.Background()) }()

	// --- handlers ----------------------------------------------------
	services := &api.ServicesConfig{Registry: reg}
	session := &api.SessionConfig{
		SigningKey:       []byte(jwtSecret),
		OperatorPassword: operatorPassword,
		// Secure cookies unless explicitly developing over plain HTTP.
		CookieSecure: envOr("COOKIE_INSECURE", "") == "",
		// So a post-deploy smoke can prove WHICH build is serving. The
		// boot log below carries the same value, but reading it needs
		// AWS credentials; this needs the operator password.
		Version: version,
	}
	auth := api.JWTMiddleware([]byte(jwtSecret))

	mux := http.NewServeMux()

	// Unauthenticated: health + the session probe + login/logout. The
	// probe MUST be outside the auth group or the SPA cannot ask
	// "am I logged in?" without already being logged in.
	//
	// /healthz carries a bare "ok" and nothing else. The build stamp
	// used to ride on this body, which handed an exact commit SHA of a
	// private repo to any anonymous caller who could reach
	// monitor.simple-forge.com. That lived here because a pin bump
	// otherwise had no outside-the-VPC verification; the version now
	// rides on the AUTHENTICATED session probe (see
	// api.SessionConfig.Version), which gives an operator the same
	// evidence without widening what an anonymous caller learns.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /api/v1/session", session.Session)
	mux.HandleFunc("POST /api/v1/login", session.Login)
	mux.HandleFunc("POST /api/v1/logout", session.Logout)

	// Prometheus text-exposition. Unauthenticated: it exposes no
	// tenant-scoped data, only process-level counters + the
	// observatory_store_backend{backend="s3"|"goembedb"} gauge an
	// operator alerts on after flipping USE_GOEMBEDB.
	mux.Handle("GET /metrics", api.MetricsHandler())

	// Migration endpoint. Disabled unless ADMIN_MIGRATION_TOKEN is
	// set — the endpoint is a one-off operator tool for the S3 →
	// go-embedb cutover, NOT a steady-state route. Source may be nil
	// if USE_GOEMBEDB=1 was deployed without STORAGE_BUCKET, in which
	// case the endpoint returns a 500 that names what is missing.
	if migrationToken != "" {
		migration := &api.MigrationConfig{
			Source:     migrationSource,
			Sink:       migrationSink,
			AdminToken: migrationToken,
		}
		mux.HandleFunc("POST /admin/migrate-s3-to-goembedb", migration.Migrate)
	}

	// Authenticated surface.
	mux.Handle("GET /api/v1/services", auth(http.HandlerFunc(services.List)))
	mux.Handle("GET /api/v1/services/{stack}", auth(http.HandlerFunc(services.Get)))
	mux.Handle("GET /api/v1/services/{stack}/deploys", auth(http.HandlerFunc(services.Deploys)))
	mux.Handle("DELETE /api/v1/services/{stack}", auth(http.HandlerFunc(services.Delete)))
	mux.Handle("POST /api/v1/services/register", auth(http.HandlerFunc(services.Register)))

	// Healthcheck sweep. The tick endpoint is authenticated like any
	// other mutating route: on Lambda the EventBridge rule carries a
	// bearer token, the same shape sc-component-backup uses for its
	// own scheduler tick.
	healthCfg := &api.HealthConfig{
		Registry:    reg,
		Prober:      health.NewHTTPProber(time.Duration(envInt("HEALTHCHECK_TIMEOUT_SECONDS", 5)) * time.Second),
		DefaultPath: envOr("HEALTHCHECK_PATH", "/healthz"),
		RetainDays:  envInt("HEALTHCHECK_RETAIN_DAYS", 30),
	}
	mux.Handle("POST /api/v1/scheduler/healthcheck/tick", auth(http.HandlerFunc(healthCfg.Tick)))
	mux.Handle("GET /api/v1/services/{stack}/health", auth(http.HandlerFunc(healthCfg.History)))

	// Telemetry. One provider per cloud, selected per service by the
	// `cloud` its record carries; a cloud with no provider answers with
	// a reason, never with an empty page.
	// One Set shared by both handlers, so the per-region clients and the
	// prefix-resolution caches are shared too: logs and metrics resolve
	// the same deploy's resources, and a second copy would double both
	// the discovery calls and the chance of the two panels disagreeing
	// about which function they are looking at.
	providers := buildProviders(ctx)
	logsCfg := &api.LogsConfig{
		Registry:      reg,
		Providers:     providers,
		DefaultWindow: time.Duration(envInt("LOGS_DEFAULT_WINDOW_MINUTES", 15)) * time.Minute,
		MaxWindow:     time.Duration(envInt("LOGS_MAX_WINDOW_HOURS", 48)) * time.Hour,
		DefaultLimit:  envInt("LOGS_PAGE_LIMIT", 200),
		MaxLimit:      envInt("LOGS_MAX_PAGE_LIMIT", 500),
		Budget: telemetry.Budget{
			// Left at the provider's defaults unless the operator
			// overrides: these bound a real CloudWatch bill and a real
			// Lambda timeout, and the safe direction is down.
			MaxEventsScanned: envInt("LOGS_MAX_EVENTS_SCANNED", 0),
			MaxCalls:         envInt("LOGS_MAX_CALLS", 0),
			MaxBytes:         envInt("LOGS_MAX_RESPONSE_KB", 0) * 1024,
		},
	}
	mux.Handle("GET /api/v1/services/{stack}/logs", auth(http.HandlerFunc(logsCfg.Logs)))

	metricsCfg := &api.MetricsConfig{
		Registry:      reg,
		Providers:     providers,
		DefaultWindow: time.Duration(envInt("METRICS_DEFAULT_WINDOW_MINUTES", 180)) * time.Minute,
		MaxWindow:     time.Duration(envInt("METRICS_MAX_WINDOW_DAYS", 30)) * 24 * time.Hour,
		// Zero leaves the provider's own fan-out cap in place; the safe
		// direction for an operator override is down.
		MaxSeries: envInt("METRICS_MAX_SERIES", 0),
	}
	mux.Handle("GET /api/v1/services/{stack}/metrics", auth(http.HandlerFunc(metricsCfg.Metrics)))

	// Embedded SPA last, on the catch-all, so it cannot shadow an API
	// route. A missing dist is a warning rather than a failure: a
	// Go-only change should not require having run npm, and an operator
	// debugging the API does not need the UI present.
	if dist, derr := ui.Dist(); derr != nil {
		slog.Warn("SPA not embedded — serving API only", "err", derr)
	} else if _, serr := fs.Stat(dist, "index.html"); serr != nil {
		slog.Warn("SPA dist present but has no index.html — serving API only", "err", serr)
	} else {
		mux.Handle("/", ui.Handler(dist))
		slog.Info("SPA mounted")
	}

	port := envOr("PORT", "8080")
	slog.Info(
		"starting sc-component-observatory",
		"version", version, "commit", commit,
		"storage", location, "metadata", metaType,
		"backend", backendLabel(useGoEmbedb),
		"telemetry_clouds", logsCfg.Providers.Clouds(),
		"operator_password_set", operatorPassword != "",
		"self_url", os.Getenv("OBSERVATORY_SELF_URL"),
		"port", port,
	)

	// Bridge through go-aws-lambda-sdk, the fleet pattern. Under Lambda
	// (AWS_LAMBDA_FUNCTION_NAME set) it joins the Lambda runtime API and
	// adapts Function URL events onto this vanilla handler; outside
	// Lambda it falls back to net/http on $PORT. One binary serves both,
	// so local development and the deployed artifact cannot drift.
	//
	// context.Background(), NOT the startup ctx: that one carries a 30s
	// timeout to bound secret resolution and store construction, and
	// handing it to the server would kill the process 30s after boot.
	svc, err := sdkservice.New(context.Background(), sdkservice.WithVanillaHandler(withRequestLog(mux)))
	if err != nil {
		return fmt.Errorf("sdkservice init: %w", err)
	}
	if err := svc.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("sdkservice exited: %w", err)
	}
	slog.Info("exited cleanly")
	return nil
}

// envInt reads an integer env var, falling back on absent or garbage
// input rather than failing the boot — these are tuning knobs, and a
// typo in one should not take the dashboard down.
func envInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("ignoring unparseable env var", "name", name, "value", v, "using", fallback)
		return fallback
	}
	return n
}

// buildProviders assembles the telemetry providers this deployment can
// serve.
//
// A provider that cannot be built is a WARNING, not a boot failure. The
// registry, the deploy timeline and the health sweep are useful without
// telemetry, and taking the whole dashboard down over a log-read
// permission the operator has not granted yet would be the component
// punishing its own adopter. The absence then surfaces per request, in
// the response, as a reason.
func buildProviders(ctx context.Context) telemetry.Set {
	set := telemetry.Set{}

	// AWS_REGION is set by the Lambda runtime; STORAGE_REGION is the
	// operator's explicit choice for the component's own bucket and the
	// best available guess otherwise. Handles carry their own region
	// when the deploy recorded one, so this is only the fallback.
	region := envOr("TELEMETRY_AWS_REGION", envOr("AWS_REGION", envOr("STORAGE_REGION", "eu-central-1")))
	awsProv, err := awsprov.New(ctx, awsprov.Config{
		DefaultRegion: region,
		// Normally empty: observatory reads its own account through the
		// execution role the template granted. Present for the
		// cross-account case, which needs a key by construction.
		AccessKey: os.Getenv("TELEMETRY_AWS_ACCESS_KEY"),
		SecretKey: os.Getenv("TELEMETRY_AWS_SECRET_KEY"),
	})
	if err != nil {
		slog.Warn("AWS telemetry provider unavailable", "err", err)
	} else {
		set[registry.CloudAWS] = awsProv
	}
	return set
}

// buildObjectStore constructs the object store from env and returns a
// human-readable location for the startup log.
func buildObjectStore(ctx context.Context) (storage.Store, string, error) {
	switch t := envOr("STORAGE_TYPE", "s3"); t {
	case "s3":
		bucket := os.Getenv("STORAGE_BUCKET")
		if bucket == "" {
			return nil, "", errors.New("STORAGE_BUCKET is required when STORAGE_TYPE=s3")
		}
		region := envOr("STORAGE_REGION", "eu-central-1")
		st, err := storage.NewS3Store(ctx, storage.S3Config{
			Bucket:       bucket,
			Region:       region,
			EndpointURL:  os.Getenv("STORAGE_ENDPOINT_URL"),
			AccessKey:    os.Getenv("STORAGE_ACCESS_KEY"),
			SecretKey:    os.Getenv("STORAGE_SECRET_KEY"),
			UsePathStyle: os.Getenv("STORAGE_USE_PATH_STYLE") != "",
		})
		if err != nil {
			return nil, "", err
		}
		return st, fmt.Sprintf("s3://%s/%s", bucket, strings.Trim(os.Getenv("STORAGE_PREFIX"), "/")), nil

	case "filesystem":
		path := os.Getenv("STORAGE_PATH")
		if path == "" {
			return nil, "", errors.New("STORAGE_PATH is required when STORAGE_TYPE=filesystem")
		}
		st, err := storage.NewFSStore(ctx, storage.FSConfig{Root: path})
		if err != nil {
			return nil, "", err
		}
		return st, "file://" + path, nil

	default:
		return nil, "", fmt.Errorf("unsupported STORAGE_TYPE %q — slice 1 ships %q and %q", t, "s3", "filesystem")
	}
}

// withRequestLog logs one line per request. Deliberately minimal: the
// registration path carries a bearer token and the login path carries a
// password, so nothing from headers or bodies is logged.
func withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info(
			"request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "elapsed", time.Since(start).String(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// backendLabel maps the boolean flag to the Prometheus label value the
// gauge records. Centralised so the boot log and the gauge agree.
func backendLabel(useGoEmbedb bool) string {
	if useGoEmbedb {
		return "goembedb"
	}
	return "s3"
}
