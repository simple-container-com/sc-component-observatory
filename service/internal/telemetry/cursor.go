// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package telemetry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Cursor is the pagination token, and it is deliberately NOT the
// provider's own.
//
// CloudWatch hands out an opaque `nextToken`, YC Logging a page token,
// the Kubernetes API a resourceVersion. Passing any of those through to
// the browser would bake one cloud's pagination model into the wire
// format and into the SPA, and the next provider would either have to
// fake it or the UI would need a per-cloud branch. So the cursor
// carries the only thing all three agree on: a timestamp boundary.
//
// It also survives what a provider token does not. A CloudWatch
// nextToken expires and is scoped to the exact query that produced it;
// a timestamp boundary can be replayed tomorrow, against a different
// filter, or after the Lambda that produced it has been recycled —
// which matters because every page here is served by a fresh,
// possibly-cold container with no memory of the last one.
type Cursor struct {
	// BeforeMS is the timestamp of the OLDEST event already delivered,
	// in epoch milliseconds. The next page is everything older, with
	// that millisecond itself re-queried (see ExcludeIDs).
	BeforeMS int64 `json:"b"`

	// ExcludeIDs are the event ids already delivered AT exactly
	// BeforeMS.
	//
	// Needed because the boundary is a millisecond, not an event: a
	// burst of lines can share a timestamp, and a page can cut through
	// the middle of one. Querying `< BeforeMS` would silently drop the
	// rest of that millisecond; querying `<= BeforeMS` would repeat
	// what was already shown. Re-querying the boundary millisecond and
	// dropping the known ids is the only variant that is neither lossy
	// nor duplicating.
	ExcludeIDs []string `json:"x,omitempty"`
}

// Encode renders the cursor for the wire. base64 of JSON, not a bare
// timestamp: an opaque token cannot be hand-edited into an unbounded
// query, and the shape stays free to grow.
func (c Cursor) Encode() string {
	b, err := json.Marshal(c)
	if err != nil {
		// Cursor is two plain fields; marshalling cannot fail. An empty
		// cursor degrades to "no more pages", never to a broken page.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a wire cursor. A malformed cursor is an error
// rather than a silent reset to "now": resetting would quietly serve
// the newest page to a caller that asked for an older one, and the
// caller would see a repeating list with no indication why.
func DecodeCursor(s string) (Cursor, error) {
	var c Cursor
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("cursor is not valid base64url: %w", err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("cursor is not valid JSON: %w", err)
	}
	if c.BeforeMS <= 0 {
		return c, fmt.Errorf("cursor carries no timestamp boundary")
	}
	return c, nil
}

// Before is the cursor boundary as a time.
func (c Cursor) Before() time.Time {
	return time.UnixMilli(c.BeforeMS).UTC()
}

// CursorFor builds the cursor that continues older than the given page.
// Returns "" when the page is empty — there is no boundary to continue
// from, and an empty cursor correctly means "no further pages".
func CursorFor(events []LogEvent) string {
	if len(events) == 0 {
		return ""
	}
	oldest := events[0].At
	for _, e := range events[1:] {
		if e.At.Before(oldest) {
			oldest = e.At
		}
	}
	ms := oldest.UnixMilli()
	c := Cursor{BeforeMS: ms}
	for _, e := range events {
		if e.At.UnixMilli() == ms && e.ID != "" {
			c.ExcludeIDs = append(c.ExcludeIDs, e.ID)
		}
	}
	return c.Encode()
}
