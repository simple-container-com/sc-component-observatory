// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package api

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// storeBackend is a labelled gauge that advertises which storage
// backend observatory is actively using.
//
// One gauge per backend label, 1 for the active one and 0 for the
// others. Operator alerting pattern:
//
//	# A deploy flipped USE_GOEMBEDB=1 but something is still going to S3
//	observatory_store_backend{backend="s3"} == 1 AND <expected>
//
// The gauge is set once at boot in main.go's run().
var (
	storeBackend = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "observatory_store_backend",
		Help: "Which storage backend the service is actively using (1 = active, 0 = inactive).",
	}, []string{"backend"})

	backendRegistered sync.Once
)

// SetStoreBackend marks `name` as the active backend and clears the
// others. Idempotent — safe to call from boot even if another
// goroutine has already recorded it.
func SetStoreBackend(name string) {
	backendRegistered.Do(func() {
		// Register lazily — the service built without this file would
		// still link cleanly, and prometheus.MustRegister panics on a
		// duplicate name if a test happens to reuse the default
		// registerer. The sync.Once bounds that.
		prometheus.MustRegister(storeBackend)
	})
	for _, b := range []string{"s3", "goembedb"} {
		if b == name {
			storeBackend.WithLabelValues(b).Set(1)
		} else {
			storeBackend.WithLabelValues(b).Set(0)
		}
	}
}

// MetricsHandler is the Prometheus text-exposition endpoint.
//
// Unauthenticated: it exposes no tenant-scoped data, only the active
// backend + whatever prometheus/client_golang's default collectors
// (Go runtime, process) register. Matches the fleet pattern — the
// aigateway and go-embedb smoke both expose /metrics without auth.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}
