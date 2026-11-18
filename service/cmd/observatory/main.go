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
// Startup is fail-fast on anything that would otherwise produce a
// service that looks healthy and answers 401 or 500 to everything —
// the first SC Component spent real time on exactly that class of
// misconfiguration.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/api"
	"github.com/simple-container-com/sc-component-observatory/service/internal/registry"
	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
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

	// --- storage -----------------------------------------------------
	objects, location, err := buildObjectStore(ctx)
	if err != nil {
		return fmt.Errorf("object store: %w", err)
	}

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
	}
	auth := api.JWTMiddleware([]byte(jwtSecret))

	mux := http.NewServeMux()

	// Unauthenticated: health + the session probe + login/logout. The
	// probe MUST be outside the auth group or the SPA cannot ask
	// "am I logged in?" without already being logged in.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","version":%q,"commit":%q}`, version, commit)
	})
	mux.HandleFunc("GET /api/v1/session", session.Session)
	mux.HandleFunc("POST /api/v1/login", session.Login)
	mux.HandleFunc("POST /api/v1/logout", session.Logout)

	// Authenticated surface.
	mux.Handle("GET /api/v1/services", auth(http.HandlerFunc(services.List)))
	mux.Handle("GET /api/v1/services/{stack}", auth(http.HandlerFunc(services.Get)))
	mux.Handle("DELETE /api/v1/services/{stack}", auth(http.HandlerFunc(services.Delete)))
	mux.Handle("POST /api/v1/services/register", auth(http.HandlerFunc(services.Register)))

	port := envOr("PORT", "8080")
	slog.Info(
		"starting sc-component-observatory",
		"version", version, "commit", commit,
		"storage", location, "metadata", metaType,
		"operator_password_set", operatorPassword != "",
		"port", port,
	)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withRequestLog(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
