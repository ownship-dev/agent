package executors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// vmState is the VM counterpart of lxcState, in its own file: an older agent never reads it,
// so it can't mistake a VM's vmid for a deleted LXC and build a fresh one.
type vmState struct {
	path string
	mu   sync.Mutex
	data vmStateData
}

type vmStateData struct {
	Workloads map[string]vmWorkload `json:"workloads"`
	// Templates imported from VM_IMAGE_DIR, by image id.
	Bases map[string]vmBase `json:"bases"`
	// Guests kept aside by a convert, by workload: Pending is the new guest until a deploy
	// promotes it, Retired the old one until commit or rollback. Neither is ever listed.
	Retired map[string]retiredGuest `json:"retired"`
	Pending map[string]retiredGuest `json:"pending"`
}

type vmWorkload struct {
	Vmid string `json:"vmid"`
	// SHA256 fingerprint of the guest's pinned SSH host key.
	HostKeyFpr string `json:"hostKeyFpr,omitempty"`
}

type vmBase struct {
	Vmid   string `json:"vmid"`
	Sha256 string `json:"sha256"`
}

type retiredGuest struct {
	Vmid string `json:"vmid"`
	Kind string `json:"kind"` // "lxc" | "vm"
}

func loadVMState(path string) (*vmState, error) {
	s := &vmState{path: path}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &s.data); err != nil {
			return nil, err
		}
	}
	if s.data.Workloads == nil {
		s.data.Workloads = map[string]vmWorkload{}
	}
	if s.data.Bases == nil {
		s.data.Bases = map[string]vmBase{}
	}
	if s.data.Retired == nil {
		s.data.Retired = map[string]retiredGuest{}
	}
	if s.data.Pending == nil {
		s.data.Pending = map[string]retiredGuest{}
	}
	return s, nil
}

func (s *vmState) get(id string) (vmWorkload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.data.Workloads[id]
	return w, ok
}

func (s *vmState) snapshot() map[string]vmWorkload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]vmWorkload, len(s.data.Workloads))
	for id, w := range s.data.Workloads {
		out[id] = w
	}
	return out
}

func (s *vmState) set(id string, w vmWorkload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Workloads[id] = w
	return s.flush()
}

// setHostKey records the pinned fingerprint for the workload holding vmid.
func (s *vmState) setHostKey(vmid, fpr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, w := range s.data.Workloads {
		if w.Vmid == vmid {
			w.HostKeyFpr = fpr
			s.data.Workloads[id] = w
		}
	}
	return s.flush()
}

func (s *vmState) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Workloads, id)
	return s.flush()
}

func (s *vmState) base(imageID string) (vmBase, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.data.Bases[imageID]
	return b, ok
}

func (s *vmState) bases() map[string]vmBase {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]vmBase, len(s.data.Bases))
	for id, b := range s.data.Bases {
		out[id] = b
	}
	return out
}

func (s *vmState) setBase(imageID string, b vmBase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Bases[imageID] = b
	return s.flush()
}

func (s *vmState) removeBase(imageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Bases, imageID)
	return s.flush()
}

func (s *vmState) hasWorkloads() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data.Workloads) > 0 || len(s.data.Retired) > 0 || len(s.data.Pending) > 0
}

func (s *vmState) retired(id string) (retiredGuest, bool) { return s.aside(s.data.Retired, id) }
func (s *vmState) pending(id string) (retiredGuest, bool) { return s.aside(s.data.Pending, id) }

func (s *vmState) setRetired(id string, g retiredGuest) error {
	return s.setAside(s.data.Retired, id, &g)
}
func (s *vmState) setPending(id string, g retiredGuest) error {
	return s.setAside(s.data.Pending, id, &g)
}
func (s *vmState) removeRetired(id string) error { return s.setAside(s.data.Retired, id, nil) }
func (s *vmState) removePending(id string) error { return s.setAside(s.data.Pending, id, nil) }

func (s *vmState) aside(m map[string]retiredGuest, id string) (retiredGuest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := m[id]
	return g, ok
}

func (s *vmState) setAside(m map[string]retiredGuest, id string, g *retiredGuest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g == nil {
		delete(m, id)
	} else {
		m[id] = *g
	}
	return s.flush()
}

// asideVmids is every vmid a convert holds, so Adopt never hands one to another workload.
func (s *vmState) asideVmids() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for _, m := range []map[string]retiredGuest{s.data.Retired, s.data.Pending} {
		for _, g := range m {
			out[g.Vmid] = true
		}
	}
	return out
}

func (s *vmState) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o600)
}
