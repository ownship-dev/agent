package executors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// lxcState persists workload to vmid, so redeploys reuse the LXC after an agent restart.
type lxcState struct {
	path  string
	mu    sync.Mutex
	Vmids map[string]string `json:"vmids"`
}

func loadLxcState(path string) (*lxcState, error) {
	s := &lxcState{path: path, Vmids: map[string]string{}}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	if s.Vmids == nil {
		s.Vmids = map[string]string{}
	}
	return s, nil
}

func (s *lxcState) get(projectID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.Vmids[projectID]
	return v, ok
}

func (s *lxcState) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.Vmids))
	for id, vmid := range s.Vmids {
		out[id] = vmid
	}
	return out
}

func (s *lxcState) projectIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.Vmids))
	for id := range s.Vmids {
		ids = append(ids, id)
	}
	return ids
}

func (s *lxcState) set(projectID, vmid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Vmids[projectID] = vmid
	return s.flush()
}

func (s *lxcState) remove(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Vmids, projectID)
	return s.flush()
}

func (s *lxcState) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o600)
}
