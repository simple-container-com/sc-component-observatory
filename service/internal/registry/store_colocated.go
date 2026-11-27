// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/simple-container-com/sc-component-observatory/service/internal/storage"
)

// ColocatedStore keeps the registry as JSON objects in the same object
// store that holds everything else, under a `_meta/` prefix.
//
// This is the DEFAULT backing store, and the reason an operator can
// deploy observatory by naming one bucket: there is no second database
// to provision, no connection string to wire, and no migration to run.
// The first SC Component shipped assuming Mongo was available and had
// to be retrofitted; starting colocated avoids that.
//
// Layout:
//
//	<prefix>/_meta/services/<enc-stack>.json          one ServiceRecord
//	<prefix>/_meta/deploys/<enc-stack>/<event-id>.json one DeployEvent
//
// Stack names contain "--" and could contain "/" on a non-fleet naming
// scheme, so keys are escaped rather than interpolated raw.
//
// Scale note: List does one LIST + one GET per service. That is fine
// for the fleet-sized deployments this targets (tens of services) and
// is why the interface keeps room for database-backed impls — an
// operator with thousands of services should point `metadata` at a
// real database instead.
type ColocatedStore struct {
	objects storage.Store
	prefix  string

	// listCache guards against the N+1 GETs being issued concurrently
	// by several dashboard requests at once. Not a correctness device —
	// just a cheap way to avoid stampeding the object store on a
	// refresh-happy browser.
	mu sync.Mutex
}

// NewColocatedStore wraps an object store. `prefix` may be empty.
func NewColocatedStore(objects storage.Store, prefix string) *ColocatedStore {
	return &ColocatedStore{objects: objects, prefix: strings.Trim(prefix, "/")}
}

const (
	metaRoot    = "_meta"
	servicesDir = "services"
	deploysDir  = "deploys"
	// deployConfigsDir keeps config snapshots in their OWN prefix rather
	// than beside the events. Sharing the prefix would make
	// DeployHistory's LIST return both and force it to filter by name —
	// a filter that is one forgotten suffix away from decoding a
	// snapshot as an event and dropping a deploy from the timeline.
	deployConfigsDir = "deploy-configs"
	jsonFileExt      = ".json"
)

// MaxHistoryScan bounds how many deploy objects one history read will
// open. A long-lived service accumulates one object per deploy forever
// and the dashboard only ever renders a page.
//
// Exported because the API layer must be able to SAY when it hit this.
// A paginated timeline that silently stops at object 500 looks exactly
// like a service whose first deploy was its 500th-from-last, and
// "absent" rendering as "that is all there is" is the failure this
// component exists to avoid.
const MaxHistoryScan = 500

// encodeStack makes a stack name safe for one path segment. Only
// characters that would change the key's shape are escaped, so the
// common case stays human-readable in a bucket listing — worth a lot
// when debugging from the AWS console.
func encodeStack(stack string) string {
	var b strings.Builder
	for _, r := range stack {
		switch {
		case r == '/' || r == '\\':
			fmt.Fprintf(&b, "~%02x", r)
		case r == '~':
			b.WriteString("~7e")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s *ColocatedStore) key(parts ...string) string {
	all := make([]string, 0, len(parts)+2)
	if s.prefix != "" {
		all = append(all, s.prefix)
	}
	all = append(all, metaRoot)
	all = append(all, parts...)
	return path.Join(all...)
}

func (s *ColocatedStore) serviceKey(stack string) string {
	return s.key(servicesDir, encodeStack(stack)+jsonFileExt)
}

func (s *ColocatedStore) deployPrefix(stack string) string {
	return s.key(deploysDir, encodeStack(stack)) + "/"
}

// putJSON marshals and writes. Uses a bytes.Reader so the object store
// can declare Content-Length — the streaming path exists for unknown
// length bodies and S3 PutObject rejects those with HTTP 411.
func (s *ColocatedStore) putJSON(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", key, err)
	}
	if _, err := s.objects.Put(ctx, key, bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// getJSON reads and unmarshals. found=false when the object is absent,
// which must be distinguishable from a backend failure: a missing
// service is a 404, a broken bucket is a 500.
func (s *ColocatedStore) getJSON(ctx context.Context, key string, out any) (bool, error) {
	rc, err := s.objects.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get %s: %w", key, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", key, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// A corrupt object should not take down the whole listing;
		// callers that can skip it do.
		return false, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return true, nil
}

func (s *ColocatedStore) Get(ctx context.Context, stack string) (ServiceRecord, bool, error) {
	var rec ServiceRecord
	found, err := s.getJSON(ctx, s.serviceKey(stack), &rec)
	if err != nil {
		return ServiceRecord{}, false, err
	}
	return rec, found, nil
}

func (s *ColocatedStore) List(ctx context.Context) ([]ServiceRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	objs, err := s.objects.List(ctx, s.key(servicesDir)+"/")
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	out := make([]ServiceRecord, 0, len(objs))
	for _, o := range objs {
		if !strings.HasSuffix(o.Path, jsonFileExt) {
			continue
		}
		var rec ServiceRecord
		found, err := s.getJSON(ctx, o.Path, &rec)
		if err != nil || !found {
			// Skip an unreadable record rather than failing the whole
			// dashboard. One corrupt object must not blank the page.
			continue
		}
		out = append(out, rec)
	}
	SortRecords(out)
	return out, nil
}

func (s *ColocatedStore) Put(ctx context.Context, rec ServiceRecord) error {
	return s.putJSON(ctx, s.serviceKey(rec.Stack), rec)
}

// Delete removes the record and its whole deploy history.
//
// Idempotent, and deliberately best-effort on the history objects: the
// operator's intent is "be rid of this service", so a single failed
// event delete must not leave the record itself behind and make them
// run it again.
func (s *ColocatedStore) Delete(ctx context.Context, stack string) error {
	for _, prefix := range []string{s.deployPrefix(stack), s.deployConfigPrefix(stack), s.healthPrefix(stack), s.healthLatestPrefix(stack)} {
		objs, err := s.objects.List(ctx, prefix)
		if err != nil {
			continue
		}
		for _, o := range objs {
			_ = s.objects.Delete(ctx, o.Path)
		}
	}
	if err := s.objects.Delete(ctx, s.serviceKey(stack)); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("delete service %s: %w", stack, err)
	}
	return nil
}

// AppendDeployEvent writes one event keyed by its id, which makes the
// write naturally idempotent: a retried registration overwrites its own
// object instead of adding a duplicate.
func (s *ColocatedStore) AppendDeployEvent(ctx context.Context, ev DeployEvent) error {
	if ev.ID == "" {
		return errors.New("deploy event has no ID")
	}
	return s.putJSON(ctx, s.deployPrefix(ev.Stack)+ev.ID+jsonFileExt, ev)
}

func (s *ColocatedStore) DeployHistory(ctx context.Context, stack string, limit int) ([]DeployEvent, error) {
	objs, err := s.objects.List(ctx, s.deployPrefix(stack))
	if err != nil {
		return nil, fmt.Errorf("list deploys: %w", err)
	}
	// Cap the fan-out (MaxHistoryScan). Newest by object mtime, which is
	// a proxy for deploy time — the authoritative order comes from the
	// decoded events below, but the choice of WHICH objects to open has
	// only the listing to go on.
	if len(objs) > MaxHistoryScan {
		sort.Slice(objs, func(i, j int) bool { return objs[i].ModifiedAt.After(objs[j].ModifiedAt) })
		objs = objs[:MaxHistoryScan]
	}
	out := make([]DeployEvent, 0, len(objs))
	for _, o := range objs {
		if !strings.HasSuffix(o.Path, jsonFileExt) {
			continue
		}
		var ev DeployEvent
		found, err := s.getJSON(ctx, o.Path, &ev)
		if err != nil || !found {
			continue
		}
		out = append(out, ev)
	}
	// Newest first, with the event id as a deterministic tiebreak for
	// events sharing a timestamp (see DeployEventID).
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].DeployedAt.Equal(out[j].DeployedAt) {
			return out[i].DeployedAt.After(out[j].DeployedAt)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *ColocatedStore) deployConfigPrefix(stack string) string {
	return s.key(deployConfigsDir, encodeStack(stack)) + "/"
}

func (s *ColocatedStore) PutDeployConfig(ctx context.Context, stack, eventID string, cfg map[string]any) error {
	if eventID == "" {
		return errors.New("deploy config has no event ID")
	}
	return s.putJSON(ctx, s.deployConfigPrefix(stack)+eventID+jsonFileExt, cfg)
}

func (s *ColocatedStore) DeployConfig(ctx context.Context, stack, eventID string) (map[string]any, bool, error) {
	if eventID == "" {
		return nil, false, nil
	}
	var cfg map[string]any
	found, err := s.getJSON(ctx, s.deployConfigPrefix(stack)+eventID+jsonFileExt, &cfg)
	if err != nil {
		return nil, false, err
	}
	return cfg, found, nil
}

func (s *ColocatedStore) Close(_ context.Context) error { return nil }

const (
	healthDir = "health"
	// healthLatestDir holds ONE object per stack and region: the newest
	// probe. It is deliberately outside `health/` so PruneHealth's sweep
	// of that prefix can never delete it, and so a history LIST never
	// returns it.
	healthLatestDir = "health-latest"
)

func (s *ColocatedStore) healthPrefix(stack string) string {
	return s.key(healthDir, encodeStack(stack)) + "/"
}

func (s *ColocatedStore) healthLatestPrefix(stack string) string {
	return s.key(healthLatestDir, encodeStack(stack)) + "/"
}

func (s *ColocatedStore) healthLatestKey(stack, region string) string {
	if region == "" {
		region = "default"
	}
	return s.healthLatestPrefix(stack) + encodeStack(region) + jsonFileExt
}

// healthObjectKey sorts lexically by time, so a prefix LIST returns
// results in chronological order without reading every object. RFC3339
// with a nanosecond suffix keeps two probes in the same second distinct.
func (s *ColocatedStore) healthObjectKey(res HealthResult) string {
	ts := res.At.UTC().Format("2006-01-02T15-04-05.000000000")
	region := res.Region
	if region == "" {
		region = "default"
	}
	return s.healthPrefix(res.Stack) + ts + "_" + encodeStack(region) + jsonFileExt
}

func (s *ColocatedStore) PutHealth(ctx context.Context, res HealthResult) error {
	if res.Stack == "" {
		return errors.New("health result has no stack")
	}
	if err := s.putJSON(ctx, s.healthObjectKey(res), res); err != nil {
		return err
	}
	// Best-effort: the history row just written is already durable, and
	// LatestHealth falls back to scanning history for a stack whose
	// pointer write failed, so a pointer error must not fail the probe.
	_ = s.putJSON(ctx, s.healthLatestKey(res.Stack, res.Region), res)
	return nil
}

func (s *ColocatedStore) HealthHistory(ctx context.Context, stack string, limit int) ([]HealthResult, error) {
	objs, err := s.objects.List(ctx, s.healthPrefix(stack))
	if err != nil {
		return nil, fmt.Errorf("list health: %w", err)
	}
	// Keys are time-ordered, so sort by key DESC and read only as many
	// as asked for. Reading every object to sort by its decoded
	// timestamp would turn a 50-row page into thousands of GETs on a
	// service that has been probed every 5 minutes for a month.
	sort.Slice(objs, func(i, j int) bool { return objs[i].Path > objs[j].Path })
	want := len(objs)
	if limit > 0 && limit < want {
		want = limit
	}
	out := make([]HealthResult, 0, want)
	for _, o := range objs {
		if len(out) >= want {
			break
		}
		if !strings.HasSuffix(o.Path, jsonFileExt) {
			continue
		}
		var res HealthResult
		found, err := s.getJSON(ctx, o.Path, &res)
		if err != nil || !found {
			continue
		}
		out = append(out, res)
	}
	return out, nil
}

// LatestHealth returns the newest probe per region.
//
// Reads the per-region pointers PutHealth maintains: one LIST over a
// handful of keys plus one GET each, regardless of how long the stack's
// history has grown. Only a stack that has no pointer yet — probed
// before pointers existed, and not probed since — pays for the history
// scan, and the next sweep gives it one.
func (s *ColocatedStore) LatestHealth(ctx context.Context, stack string) (map[string]HealthResult, error) {
	objs, err := s.objects.List(ctx, s.healthLatestPrefix(stack))
	if err != nil {
		return nil, fmt.Errorf("list latest health: %w", err)
	}
	out := map[string]HealthResult{}
	for _, o := range objs {
		if !strings.HasSuffix(o.Path, jsonFileExt) {
			continue
		}
		var res HealthResult
		found, err := s.getJSON(ctx, o.Path, &res)
		if err != nil || !found {
			continue
		}
		out[res.Region] = res
	}
	if len(out) > 0 {
		return out, nil
	}
	return s.latestHealthFromHistory(ctx, stack)
}

func (s *ColocatedStore) latestHealthFromHistory(ctx context.Context, stack string) (map[string]HealthResult, error) {
	// One page is enough to cover every region's latest result in any
	// realistic deployment, and bounds the GETs.
	hist, err := s.HealthHistory(ctx, stack, 50)
	if err != nil {
		return nil, err
	}
	out := map[string]HealthResult{}
	for _, r := range hist {
		if _, seen := out[r.Region]; !seen {
			out[r.Region] = r
		}
	}
	return out, nil
}

// PruneHealth deletes results older than `before`.
//
// Relies on the key's time prefix rather than decoding each object, so
// the sweep costs one LIST plus a DELETE per expired key.
func (s *ColocatedStore) PruneHealth(ctx context.Context, before time.Time) (int, error) {
	objs, err := s.objects.List(ctx, s.key(healthDir)+"/")
	if err != nil {
		return 0, fmt.Errorf("list health for prune: %w", err)
	}
	cutoff := before.UTC().Format("2006-01-02T15-04-05.000000000")
	pruned := 0
	for _, o := range objs {
		base := path.Base(o.Path)
		// Compare the lexical time prefix; same ordering property the
		// key format is chosen for.
		if i := strings.Index(base, "_"); i > 0 && base[:i] < cutoff {
			if err := s.objects.Delete(ctx, o.Path); err == nil {
				pruned++
			}
		}
	}
	return pruned, nil
}
