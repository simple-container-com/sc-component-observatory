// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// Internal test — package storage, not storage_test. listProjection is
// unexported on purpose (nothing outside this file should choose the
// field set), and the property being asserted is a property of the
// QUERY, not of its result.
package storage

import "testing"

// TestListProjection_OmitsBody is a tripwire, and deliberately couples
// to the implementation.
//
// The property is "a listing must not transfer document payloads", and
// that is unobservable from List's return value: the bodies are fetched,
// decoded, and discarded, so a List that carries every payload returns
// byte-identical results to one that carries none. It shows up only as
// latency, which is exactly how it survived into production — the
// cutover measurement was 10.4-11.4 s for 8 dashboard rows, and the
// correctness tests were all green.
//
// A latency assertion here would be flaky and would not name the cause.
// So this asserts the field set instead, and the failure message says
// what re-adding `body` costs. See listProjection for the full account.
func TestListProjection_OmitsBody(t *testing.T) {
	proj := listProjection()

	if _, present := proj["body"]; present {
		t.Error("listProjection includes `body`: every List will now transfer the full payload of every matching document and discard it. " +
			"On the dashboard's hot path (LatestHealth per service, ~1112 health keys per stack) that is ~8900 documents per request and took /api/v1/services from sub-second to 10.4s. " +
			"If a caller needs sizes, add a stored `size` field written at Put time rather than projecting the body.")
	}

	// The two fields List actually reads. Asserted so that trimming the
	// projection further — the other direction of the same mistake —
	// fails here rather than silently zeroing ModifiedAt, which the
	// health prune sweep orders by.
	for _, f := range []string{"_id", "updatedAt"} {
		if _, present := proj[f]; !present {
			t.Errorf("listProjection is missing %q, which List decodes into StoredObject", f)
		}
	}
}
