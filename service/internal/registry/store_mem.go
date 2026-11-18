// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package registry

import (
	"context"
	"sort"
	"sync"
)

// InMemoryStore is the tests + local-dev Store. No persistence.
type InMemoryStore struct {
	mu      sync.RWMutex
	records map[string]ServiceRecord
	events  map[string][]DeployEvent // keyed by stack
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		records: map[string]ServiceRecord{},
		events:  map[string][]DeployEvent{},
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
	return nil
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
