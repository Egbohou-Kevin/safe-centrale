package filelock

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Holders exclude each other, in one process too, and a lock held too long is
// reported, not waited for forever.
func TestHoldersTakeTurns(t *testing.T) {
	dir := t.TempDir()
	var inside, most atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := Hold(dir, "codes.lock", 10*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
			}
			time.Sleep(5 * time.Millisecond)
			inside.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Fatalf("%d holders at once", most.Load())
	}
	release, err := Hold(dir, "codes.lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := Hold(dir, "codes.lock", 50*time.Millisecond); err == nil {
		t.Fatal("a held lock taken again")
	}
}
