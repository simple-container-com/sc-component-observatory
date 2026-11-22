// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package telemetry

import (
	"strings"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	in := Cursor{BeforeMS: 1763812800123, ExcludeIDs: []string{"a", "b"}}
	out, err := DecodeCursor(in.Encode())
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if out.BeforeMS != in.BeforeMS {
		t.Errorf("BeforeMS = %d, want %d", out.BeforeMS, in.BeforeMS)
	}
	if strings.Join(out.ExcludeIDs, ",") != "a,b" {
		t.Errorf("ExcludeIDs = %v, want [a b]", out.ExcludeIDs)
	}
}

// TestCursorIsURLSafe — the cursor travels as a query parameter, so a
// `+` or `/` from standard base64 would arrive corrupted (or, worse,
// decode to a different boundary) after a round trip through a browser.
func TestCursorIsURLSafe(t *testing.T) {
	for i := 0; i < 64; i++ {
		enc := Cursor{BeforeMS: int64(1763812800000 + i*7919)}.Encode()
		if strings.ContainsAny(enc, "+/=") {
			t.Fatalf("cursor %q contains a character that needs URL escaping", enc)
		}
	}
}

// TestDecodeCursorRejectsGarbage. A malformed cursor must NOT quietly
// reset to "now": that serves the newest page to a caller asking for an
// older one, which renders as a list that repeats forever with nothing
// explaining why.
func TestDecodeCursorRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"not base64":     "!!!!not-base64!!!!",
		"not json":       "aGVsbG8gd29ybGQ", // "hello world"
		"no boundary":    Cursor{}.Encode(),
		"negative":       Cursor{BeforeMS: -5}.Encode(),
		"empty is a bug": "",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCursor(raw); err == nil {
				t.Errorf("DecodeCursor(%q) accepted a bad cursor", raw)
			}
		})
	}
}

func TestCursorBefore(t *testing.T) {
	want := time.Date(2026, 11, 22, 12, 0, 0, 123_000_000, time.UTC)
	got := Cursor{BeforeMS: want.UnixMilli()}.Before()
	if !got.Equal(want) {
		t.Errorf("Before() = %s, want %s", got, want)
	}
}

// TestCursorForCollectsTheWholeBoundaryMillisecond is the property the
// whole design rests on: every id at the oldest millisecond must be
// carried, or the next page either repeats or drops a line.
func TestCursorForCollectsTheWholeBoundaryMillisecond(t *testing.T) {
	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	page := []LogEvent{
		{At: at.Add(2 * time.Second), ID: "newest"},
		{At: at, ID: "x"},
		{At: at, ID: "y"},
		{At: at.Add(time.Second), ID: "mid"},
	}
	c, err := DecodeCursor(CursorFor(page))
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if c.BeforeMS != at.UnixMilli() {
		t.Errorf("BeforeMS = %d, want the OLDEST event's ms %d", c.BeforeMS, at.UnixMilli())
	}
	ids := strings.Join(c.ExcludeIDs, ",")
	if !strings.Contains(ids, "x") || !strings.Contains(ids, "y") {
		t.Errorf("ExcludeIDs = %v, want both ids at the boundary ms", c.ExcludeIDs)
	}
	for _, unwanted := range []string{"newest", "mid"} {
		if strings.Contains(ids, unwanted) {
			t.Errorf("ExcludeIDs carries %q, which is not at the boundary ms — excluding it would hide an event the next page should not see anyway", unwanted)
		}
	}
}

// TestCursorForEmptyPage — no boundary to continue from, and an empty
// cursor correctly means "no further pages".
func TestCursorForEmptyPage(t *testing.T) {
	if got := CursorFor(nil); got != "" {
		t.Errorf("CursorFor(nil) = %q, want empty", got)
	}
}

// TestCursorForSkipsEmptyIDs — a provider that does not supply event
// ids still gets a usable boundary; it just cannot de-duplicate inside
// the millisecond, and an empty string in ExcludeIDs would match every
// such event and silently drop them all.
func TestCursorForSkipsEmptyIDs(t *testing.T) {
	at := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	c, err := DecodeCursor(CursorFor([]LogEvent{{At: at}, {At: at}}))
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if len(c.ExcludeIDs) != 0 {
		t.Errorf("ExcludeIDs = %v, want none when the provider supplied no ids", c.ExcludeIDs)
	}
}
