package executors

import (
	"context"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"sync"
)

type engineEntry struct {
	Kind   EngineKind `json:"kind"`
	Asleep bool       `json:"asleep,omitempty"`
	// The old engine after a switch; its volumes go once the new one is healthy.
	PendingOld EngineKind `json:"pendingOld,omitempty"`
}

// engineState records each workload's engine, so commands without one reach the right CLI
// after a restart.
type engineState struct {
	path    string
	mu      sync.Mutex
	entries map[string]engineEntry
	engines map[EngineKind]*Engine
	order   []*Engine
	probe   func(ctx context.Context, e *Engine, workloadID string) bool
	// detect finds engines that came up after the agent; nil means engines are fixed.
	detect func(k EngineKind) *Engine
}

func loadEngineState(path string, engines []*Engine, probe func(context.Context, *Engine, string) bool) *engineState {
	s := &engineState{path: path, entries: map[string]engineEntry{}, engines: map[EngineKind]*Engine{}, order: engines, probe: probe}
	for _, e := range engines {
		s.engines[e.Kind] = e
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.entries)
	}
	return s
}

func (s *engineState) Engines() []*Engine {
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Engine(nil), s.order...)
}

// Engine is not ok for a kind this host doesn't have yet.
func (s *engineState) Engine(k EngineKind) (*Engine, bool) {
	k = normalizeKind(k)
	s.mu.Lock()
	e, ok := s.engines[k]
	s.mu.Unlock()
	if !ok && s.detect != nil {
		if e = s.detect(k); e != nil {
			s.add(e)
			return e, true
		}
	}
	return e, ok
}

// refresh picks up engines that weren't answering at start.
func (s *engineState) refresh() {
	if s.detect == nil {
		return
	}
	for _, k := range []EngineKind{KindDocker, KindPodman, KindRootless} {
		s.mu.Lock()
		_, have := s.engines[k]
		s.mu.Unlock()
		if !have {
			if e := s.detect(k); e != nil {
				s.add(e)
			}
		}
	}
}

func (s *engineState) add(e *Engine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.engines[e.Kind]; !ok {
		s.engines[e.Kind] = e
		s.order = append(s.order, e)
	}
}

func (s *engineState) missingRecorded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if _, ok := s.engines[normalizeKind(e.Kind)]; !ok {
			return true
		}
	}
	return false
}

// byKind gives a vanished engine its own CLI, so commands fail loudly instead of hitting Docker.
func (s *engineState) byKind(k EngineKind) *Engine {
	k = normalizeKind(k)
	s.mu.Lock()
	e, ok := s.engines[k]
	s.mu.Unlock()
	if ok {
		return e
	}
	switch k {
	case KindPodman:
		return NewPodmanEngine()
	case KindRootless:
		u, err := user.Lookup(RootlessUser)
		if err != nil {
			return s.unresolvableRootless(err.Error())
		}
		e, err := NewRootlessPodmanEngine(u)
		if err != nil {
			return s.unresolvableRootless(err.Error())
		}
		return e
	default:
		return NewDockerEngine()
	}
}

// unresolvableRootless fails every command: as root, podman would use the rootful store.
func (s *engineState) unresolvableRootless(why string) *Engine {
	return &Engine{Kind: KindRootless, Bin: "podman", Rootless: true,
		unavailable: "rootless Podman's user " + RootlessUser + " is not usable: " + why}
}

func (s *engineState) For(ctx context.Context, workloadID string) *Engine {
	s.mu.Lock()
	entry, ok := s.entries[workloadID]
	s.mu.Unlock()
	if ok {
		return s.byKind(entry.Kind)
	}
	if s.probe != nil {
		for _, e := range s.Engines() {
			if s.probe(ctx, e, workloadID) {
				_ = s.Set(workloadID, e.Kind)
				return e
			}
		}
	}
	return s.byKind(KindDocker)
}

func (s *engineState) Recorded(workloadID string) (EngineKind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[workloadID]
	return e.Kind, ok
}

func (s *engineState) update(workloadID string, f func(*engineEntry)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[workloadID]
	if !ok {
		e.Kind = KindDocker
	}
	f(&e)
	s.entries[workloadID] = e
	return s.flush()
}

func (s *engineState) Set(workloadID string, k EngineKind) error {
	return s.update(workloadID, func(e *engineEntry) { e.Kind = normalizeKind(k) })
}

func (s *engineState) SetAsleep(workloadID string, asleep bool) error {
	return s.update(workloadID, func(e *engineEntry) { e.Asleep = asleep })
}

func (s *engineState) SetPendingOld(workloadID string, k EngineKind) error {
	return s.update(workloadID, func(e *engineEntry) { e.PendingOld = normalizeKind(k) })
}

func (s *engineState) ClearPendingOld(workloadID string) error {
	return s.update(workloadID, func(e *engineEntry) { e.PendingOld = 0 })
}

func (s *engineState) PendingOld(workloadID string) (EngineKind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[workloadID]
	return e.PendingOld, e.PendingOld != 0
}

func (s *engineState) Asleep() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, e := range s.entries {
		if e.Asleep {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func (s *engineState) Remove(workloadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, workloadID)
	return s.flush()
}

func (s *engineState) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s.entries)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o600)
}
