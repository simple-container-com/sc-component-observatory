// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// pagingListAPI is a ListObjectsV2 fake that behaves like S3 does: at
// most pageSize keys per call, IsTruncated + NextContinuationToken
// while more remain, and — the part that matters — it REFUSES a
// continuation token it did not mint. A fake that ignores the token
// cannot tell a correct pagination loop from one that re-requests the
// first page forever.
type pagingListAPI struct {
	objs     []types.Object
	pageSize int

	// calls records the ContinuationToken of every request, in order.
	// nil for the first page; the string value afterwards.
	calls []*string

	// prefixes records the Prefix of every request, in order.
	prefixes []string

	// issued maps a minted token to the offset it stands for, so an
	// unrecognised token is an error rather than a silent restart.
	issued map[string]int
}

func (p *pagingListAPI) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	p.calls = append(p.calls, in.ContinuationToken)
	p.prefixes = append(p.prefixes, aws.ToString(in.Prefix))

	offset := 0
	if tok := aws.ToString(in.ContinuationToken); tok != "" {
		off, ok := p.issued[tok]
		if !ok {
			return nil, fmt.Errorf("fake s3: unknown continuation token %q", tok)
		}
		offset = off
	}

	end := min(offset+p.pageSize, len(p.objs))
	out := &s3.ListObjectsV2Output{Contents: p.objs[offset:end]}
	if end < len(p.objs) {
		if p.issued == nil {
			p.issued = map[string]int{}
		}
		tok := fmt.Sprintf("tok-%d", end)
		p.issued[tok] = end
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(tok)
	} else {
		out.IsTruncated = aws.Bool(false)
	}
	return out, nil
}

func fixtureObjects(n int) []types.Object {
	objs := make([]types.Object, 0, n)
	base := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	for i := range n {
		objs = append(objs, types.Object{
			// Zero-padded so lexical order == numeric order, the way
			// real keys sort and the way the migration iterates.
			Key:          aws.String(fmt.Sprintf("health/%05d.json", i)),
			Size:         aws.Int64(int64(i)),
			LastModified: aws.Time(base.Add(time.Duration(i) * time.Second)),
		})
	}
	return objs
}

// TestS3Store_List_PaginatesPastPageCap is the regression test for the
// unpaginated-list bug: a single ListObjectsV2 returns at most 1000
// keys, and the old List read exactly one page and dropped the rest
// without erroring. 1001 is the only interesting fixture size — it is
// the first count a one-page lister gets wrong.
func TestS3Store_List_PaginatesPastPageCap(t *testing.T) {
	const total = 1001
	api := &pagingListAPI{objs: fixtureObjects(total), pageSize: 1000}

	got, err := listAllObjects(context.Background(), api, "bkt", "health/")
	if err != nil {
		t.Fatalf("listAllObjects: %v", err)
	}

	if len(got) != total {
		t.Fatalf("got %d objects, want %d (a 1000 here means the listing was truncated at the page cap)", len(got), total)
	}
	// Spot-check both ends: the first page's head and the key that
	// only exists on page 2.
	if got[0].Path != "health/00000.json" {
		t.Errorf("first key = %q, want health/00000.json", got[0].Path)
	}
	if got[total-1].Path != "health/01000.json" {
		t.Errorf("last key = %q, want health/01000.json", got[total-1].Path)
	}
	// The per-object metadata came from the right object, not just from
	// some object. The fixture stamps LastModified at base+i seconds, so
	// an exact compare here distinguishes "page 2 was stitched on" from
	// "page 2's keys were stitched onto page 1's metadata". This used to
	// assert on StoredObject.Size, which no longer exists — see the
	// comment on StoredObject.
	wantMod := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC).Add(time.Duration(total-1) * time.Second)
	if !got[total-1].ModifiedAt.Equal(wantMod) {
		t.Errorf("last ModifiedAt = %s, want %s", got[total-1].ModifiedAt, wantMod)
	}

	// Two calls, and the second one must carry the token the FIRST
	// call handed back — not nil, not empty, not a token of its own
	// invention. A loop that re-requests page 1 returns 2000 objects
	// and would pass a count-only assertion.
	if len(api.calls) != 2 {
		t.Fatalf("made %d ListObjectsV2 calls, want 2", len(api.calls))
	}
	if api.calls[0] != nil {
		t.Errorf("first call sent ContinuationToken %q, want nil", aws.ToString(api.calls[0]))
	}
	if tok := aws.ToString(api.calls[1]); tok != "tok-1000" {
		t.Errorf("second call sent ContinuationToken %q, want the token page 1 returned (tok-1000)", tok)
	}
}

// TestS3Store_List_SinglePageDoesNotLoop guards the other direction:
// an untruncated response must end the loop. Trusting the presence of
// NextContinuationToken instead of IsTruncated, or looping while the
// token is non-nil, spins forever against a real bucket.
func TestS3Store_List_SinglePageDoesNotLoop(t *testing.T) {
	api := &pagingListAPI{objs: fixtureObjects(3), pageSize: 1000}

	got, err := listAllObjects(context.Background(), api, "bkt", "")
	if err != nil {
		t.Fatalf("listAllObjects: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d objects, want 3", len(got))
	}
	if len(api.calls) != 1 {
		t.Fatalf("made %d ListObjectsV2 calls, want 1", len(api.calls))
	}
}

// TestS3Store_List_ErrorOnLaterPage — a failure on page 2 must surface,
// not return the partial first page as if it were the whole bucket. A
// short list that reads as success is exactly how the migration's
// `verified` (sinkRows >= s3Count) came to be computable as true on a
// clipped source.
func TestS3Store_List_ErrorOnLaterPage(t *testing.T) {
	api := &failOnSecondPage{pagingListAPI{objs: fixtureObjects(1001), pageSize: 1000}}

	got, err := listAllObjects(context.Background(), api, "bkt", "health/")
	if err == nil {
		t.Fatalf("want error from page 2, got %d objects and nil error", len(got))
	}
	if got != nil {
		t.Errorf("want nil result alongside the error, got %d objects", len(got))
	}
}

type failOnSecondPage struct{ pagingListAPI }

func (f *failOnSecondPage) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if in.ContinuationToken != nil {
		return nil, errors.New("boom")
	}
	return f.pagingListAPI.ListObjectsV2(ctx, in, optFns...)
}

// TestS3Store_List_TrimsLeadingSlash pins the prefix normalisation that
// moved into listAllObjects with the pagination loop.
func TestS3Store_List_TrimsLeadingSlash(t *testing.T) {
	api := &pagingListAPI{objs: fixtureObjects(1), pageSize: 1000}

	if _, err := listAllObjects(context.Background(), api, "bkt", "/health/"); err != nil {
		t.Fatalf("listAllObjects: %v", err)
	}
	// The fake ignores Prefix for selection, so assert on what was sent.
	if len(api.prefixes) != 1 {
		t.Fatalf("made %d calls, want 1", len(api.prefixes))
	}
	if api.prefixes[0] != "health/" {
		t.Errorf("sent Prefix %q, want health/ (leading slash not trimmed)", api.prefixes[0])
	}
}

// TestS3Store_List_PaginatedPrefixIsStable — every page must carry the
// same (trimmed) prefix. Sending it only on the first request, or
// re-trimming per page, would make page 2 list the whole bucket.
func TestS3Store_List_PaginatedPrefixIsStable(t *testing.T) {
	api := &pagingListAPI{objs: fixtureObjects(1001), pageSize: 1000}

	if _, err := listAllObjects(context.Background(), api, "bkt", "/health/"); err != nil {
		t.Fatalf("listAllObjects: %v", err)
	}
	if len(api.prefixes) != 2 {
		t.Fatalf("made %d calls, want 2", len(api.prefixes))
	}
	for i, p := range api.prefixes {
		if p != "health/" {
			t.Errorf("call %d sent Prefix %q, want health/", i, p)
		}
	}
}
