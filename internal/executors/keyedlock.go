package executors

import "sync"

// keyedLocks is a per-key mutex map that evicts unused keys, so memory doesn't grow with every
// workload ever deployed.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*refCountedMutex
}

type refCountedMutex struct {
	mu   sync.Mutex
	refs int // guarded by keyedLocks.mu
}

func newKeyedLocks() *keyedLocks {
	return &keyedLocks{locks: map[string]*refCountedMutex{}}
}

// lock returns a release func that must be called exactly once.
func (l *keyedLocks) lock(key string) func() {
	l.mu.Lock()
	e, ok := l.locks[key]
	if !ok {
		e = &refCountedMutex{}
		l.locks[key] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}
