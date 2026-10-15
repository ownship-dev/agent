package executors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// serviceState persists workload to service URL, so RouteUpdate works after a restart.
type serviceState struct {
	path     string
	mu       sync.Mutex
	Services map[string]string `json:"services"`
}

func loadServiceState(path string) *serviceState {
	s := &serviceState{path: path, Services: map[string]string{}}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s)
	}
	if s.Services == nil {
		s.Services = map[string]string{}
	}
	return s
}

func (s *serviceState) get(workloadID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.Services[workloadID]
	return v, ok
}

func (s *serviceState) set(workloadID, service string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Services[workloadID] = service
	_ = s.flush()
}

func (s *serviceState) remove(workloadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Services, workloadID)
	_ = s.flush()
}

func (s *serviceState) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o600)
}
