package executors

import (
	"sync"
	"testing"
)

func TestKeyedLocksEvictsAfterRelease(t *testing.T) {
	l := newKeyedLocks()
	release := l.lock("a")
	if len(l.locks) != 1 {
		t.Fatalf("locks map = %d entries, want 1 while held", len(l.locks))
	}
	release()
	if len(l.locks) != 0 {
		t.Fatalf("locks map = %d entries, want 0 after release", len(l.locks))
	}
}

func TestKeyedLocksSerializesSameKey(t *testing.T) {
	l := newKeyedLocks()
	var mu sync.Mutex
	inside := 0
	maxConcurrent := 0
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := l.lock("shared")
			defer release()
			mu.Lock()
			inside++
			if inside > maxConcurrent {
				maxConcurrent = inside
			}
			mu.Unlock()
			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxConcurrent != 1 {
		t.Fatalf("max concurrent holders of the same key = %d, want 1", maxConcurrent)
	}
	if len(l.locks) != 0 {
		t.Fatalf("locks map = %d entries, want 0 once every goroutine released", len(l.locks))
	}
}

func TestKeyedLocksDoesNotEvictWhileAnotherWaiter(t *testing.T) {
	l := newKeyedLocks()
	release1 := l.lock("a")

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		release2 := l.lock("a")
		release2()
		close(done)
	}()
	<-started
	// Best-effort chance for the second goroutine to wait; the assertion only checks the final state.
	release1()
	<-done
	if len(l.locks) != 0 {
		t.Fatalf("locks map = %d entries, want 0 once both released", len(l.locks))
	}
}
