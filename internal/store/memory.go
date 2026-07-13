package store

import (
	"context"
	"sort"
	"sync"

	"github.com/esosaoh/crawl/internal/engine"
)

// Memory is the default zero-dependency store. Link state lives only for the
// process lifetime, so incremental re-scans work within one server session.
type Memory struct {
	mu     sync.Mutex
	scans  map[string]*ScanRecord
	states map[string]*engine.LinkState
}

func NewMemory() *Memory {
	return &Memory{
		scans:  make(map[string]*ScanRecord),
		states: make(map[string]*engine.LinkState),
	}
}

func (m *Memory) GetStates(_ context.Context, urls []string) (map[string]*engine.LinkState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*engine.LinkState)
	for _, u := range urls {
		if s, ok := m.states[u]; ok {
			cp := *s
			out[u] = &cp
		}
	}
	return out, nil
}

func (m *Memory) PutStates(_ context.Context, states []*engine.LinkState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range states {
		cp := *s
		m.states[s.URL] = &cp
	}
	return nil
}

func (m *Memory) CreateScan(_ context.Context, rec *ScanRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scans[rec.ID] = rec
	return nil
}

func (m *Memory) UpdateScan(ctx context.Context, rec *ScanRecord) error {
	return m.CreateScan(ctx, rec)
}

func (m *Memory) GetScan(_ context.Context, id string) (*ScanRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.scans[id]
	if !ok {
		return nil, ErrNotFound
	}
	return rec, nil
}

func (m *Memory) ListScans(_ context.Context, limit int) ([]*ScanRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*ScanRecord, 0, len(m.scans))
	for _, rec := range m.scans {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) Close(context.Context) error { return nil }
