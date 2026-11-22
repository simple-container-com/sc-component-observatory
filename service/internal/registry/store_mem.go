// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package registry

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// InMemoryStore is the tests + local-dev Store. No persistence.
type InMemoryStore struct {
	mu      sync.RWMutex
	records map[string]ServiceRecord
	events  map[string][]DeployEvent  // keyed by stack
	health  map[string][]HealthResult // keyed by stack
	// configs are deploy config snapshots, keyed by stack then event id
	// — mirroring the colocated store's separate prefix so the two
	// implementations cannot disagree about whether a snapshot exists.
	configs map[string]map[string]map[string]any
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		records: map[string]ServiceRecord{},
		events:  map[string][]DeployEvent{},
		health:  map[string][]HealthResult{},
		configs: map[string]map[string]map[string]any{},
	}
}

func (s *InMemoryStore) Get(_ context.Context, stack string) (ServiceRecord, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[stack]
	return r, ok, nil
}

func (s *InMemoryStore) List(_ context.Context) ([]ServiceRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ServiceRecord, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	SortRecords(out)
	return out, nil
}

func (s *InMemoryStore) Put(_ context.Context, rec ServiceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[rec.Stack] = rec
	return nil
}

func (s *InMemoryStore) Delete(_ context.Context, stack string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, stack)
	delete(s.events, stack)
	delete(s.health, stack)
	delete(s.configs, stack)
	return nil
}

// PutDeployConfig keeps the snapshot by reference — this store backs
// tests and local runs, where the caller does not mutate the config
// after registering it. It rejects an empty event id for the same
// reason the colocated store does: an unkeyed snapshot is unreachable,
// and the two implementations must agree on what they refuse.
func (s *InMemoryStore) PutDeployConfig(_ context.Context, stack, eventID string, cfg map[string]any) error {
	if eventID == "" {
		return errors.New("deploy config has no event ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configs == nil {
		s.configs = map[string]map[string]map[string]any{}
	}
	if s.configs[stack] == nil {
		s.configs[stack] = map[string]map[string]any{}
	}
	s.configs[stack][eventID] = cfg
	return nil
}

func (s *InMemoryStore) DeployConfig(_ context.Context, stack, eventID string) (map[string]any, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg, ok := s.configs[stack][eventID]
	return cfg, ok, nil
}

func (s *InMemoryStore) AppendDeployEvent(_ context.Context, ev DeployEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Idempotent on ID — the plugin retries registrations.
	for _, existing := range s.events[ev.Stack] {
		if existing.ID == ev.ID {
			return nil
		}
	}
	s.events[ev.Stack] = append(s.events[ev.Stack], ev)
	return nil
}

func (s *InMemoryStore) DeployHistory(_ context.Context, stack string, limit int) ([]DeployEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.events[stack]

	// Newest first, with insertion order as the tiebreak.
	//
	// Sorting on DeployedAt alone is not a total order: two deploys can
	// share a timestamp whenever the clock feeding them is coarse (and
	// an injected test clock is maximally coarse), and sort.Slice is not
	// stable, so the timeline would render in an arbitrary order —
	// showing an older version as the current deploy. Copy in reverse so
	// the most recently appended event wins a tie, then stable-sort.
	out := make([]DeployEvent, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		out = append(out, src[i])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DeployedAt.After(out[j].DeployedAt) })

	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *InMemoryStore) Close(_ context.Context) error { return nil }

func (s *InMemoryStore) PutHealth(_ context.Context, res HealthResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.health == nil {
		s.health = map[string][]HealthResult{}
	}
	s.health[res.Stack] = append(s.health[res.Stack], res)
	return nil
}

func (s *InMemoryStore) HealthHistory(_ context.Context, stack string, limit int) ([]HealthResult, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.health[stack]
	// Reverse-copy then stable-sort so ties on an identical timestamp
	// resolve to most-recently-recorded first, same reasoning as
	// DeployHistory.
	out := make([]HealthResult, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		out = append(out, src[i])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *InMemoryStore) LatestHealth(ctx context.Context, stack string) (map[string]HealthResult, error) {
	hist, err := s.HealthHistory(ctx, stack, 0)
	if err != nil {
		return nil, err
	}
	out := map[string]HealthResult{}
	// hist is newest-first, so the first sighting of a region wins.
	for _, r := range hist {
		if _, seen := out[r.Region]; !seen {
			out[r.Region] = r
		}
	}
	return out, nil
}

func (s *InMemoryStore) PruneHealth(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pruned := 0
	for stack, results := range s.health {
		kept := results[:0]
		for _, r := range results {
			if r.At.Before(before) {
				pruned++
				continue
			}
			kept = append(kept, r)
		}
		s.health[stack] = kept
	}
	return pruned, nil
}
