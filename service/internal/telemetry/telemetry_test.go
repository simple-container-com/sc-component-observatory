// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package telemetry

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
)

type stubProvider struct{ name string }

func (s stubProvider) Name() string                 { return s.name }
func (s stubProvider) Capabilities() Capabilities   { return Capabilities{Logs: true} }
func (s stubProvider) Health(context.Context) error { return nil }
func (s stubProvider) Logs(context.Context, LogQuery) (LogPage, error) {
	return LogPage{Events: []LogEvent{}, Queried: []string{}, Warnings: []string{}}, nil
}

func (s stubProvider) Metrics(context.Context, MetricQuery) ([]Series, error) {
	return nil, Unsupported(s.name, "metrics")
}

func (s stubProvider) Workloads(context.Context, WorkloadQuery) ([]Workload, error) {
	return nil, Unsupported(s.name, "live workload state")
}

// TestSetFor covers the three ways a provider lookup can come up empty.
// All three must produce an UnavailableError carrying an operator-facing
// reason, because that string is what the dashboard renders verbatim —
// a lookup that returned (nil, nil) would render as "no telemetry" for a
// fleet whose telemetry is merely unwired.
func TestSetFor(t *testing.T) {
	set := Set{"aws": stubProvider{name: "aws"}, "yandex": nil}

	t.Run("configured", func(t *testing.T) {
		p, err := set.For("aws")
		if err != nil {
			t.Fatalf("For(aws): %v", err)
		}
		if p.Name() != "aws" {
			t.Errorf("Name = %q", p.Name())
		}
	})

	for name, cloud := range map[string]string{
		"no cloud recorded": "",
		"cloud not wired":   "gcp",
		"explicitly nil":    "yandex",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := set.For(cloud)
			var unav *UnavailableError
			if !errors.As(err, &unav) {
				t.Fatalf("want an UnavailableError, got %v", err)
			}
			if strings.TrimSpace(unav.Reason) == "" {
				t.Fatal("an UnavailableError with no reason is the silent degradation this type exists to prevent")
			}
			if cloud != "" && !strings.Contains(unav.Reason, cloud) {
				t.Errorf("reason does not name the cloud it could not serve: %q", unav.Reason)
			}
		})
	}
}

// TestSetClouds skips nil entries — a cloud key with no implementation
// behind it would otherwise be logged at startup as configured.
func TestSetClouds(t *testing.T) {
	got := Set{"aws": stubProvider{name: "aws"}, "yandex": nil, "kubernetes": stubProvider{name: "kubernetes"}}.Clouds()
	sort.Strings(got)
	if strings.Join(got, ",") != "aws,kubernetes" {
		t.Errorf("Clouds() = %v, want [aws kubernetes]", got)
	}
}

func TestSetCloudsOnNilSetIsEmptyNotNil(t *testing.T) {
	if got := Set(nil).Clouds(); got == nil {
		t.Error("Clouds() must return a real slice — a nil one marshals to JSON null and breaks a client that spreads it")
	}
}

// TestUnsupportedIsUnavailableNotAFailure — a capability that has not
// shipped must flow through the HTTP layer's 200 path, which keys off
// the error TYPE. If Unsupported ever returns a plain error the
// dashboard starts showing a red box for "metrics land in slice 3".
func TestUnsupportedIsUnavailableNotAFailure(t *testing.T) {
	err := Unsupported("the AWS provider", "metrics")
	var unav *UnavailableError
	if !errors.As(err, &unav) {
		t.Fatalf("want an UnavailableError, got %T", err)
	}
	if !strings.Contains(unav.Reason, "the AWS provider") || !strings.Contains(unav.Reason, "metrics") {
		t.Errorf("reason names neither the provider nor the capability: %q", unav.Reason)
	}
	if err.Error() != unav.Reason {
		t.Errorf("Error() = %q, want the reason verbatim", err.Error())
	}
}
