//go:build linux

package network

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEndpointSemaphore_SingleSlot(t *testing.T) {
	dir := t.TempDir()
	sem := newEndpointSemaphore(dir, 1)

	// First acquire succeeds immediately.
	release1, err := sem.Acquire()
	require.NoError(t, err)

	// Second acquire should block until first is released.
	acquired := make(chan struct{})
	go func() {
		release2, err := sem.Acquire()
		assert.NoError(t, err)
		close(acquired)
		release2()
	}()

	// Give the goroutine time to start blocking.
	select {
	case <-acquired:
		t.Fatal("second acquire should block while slot is held")
	case <-time.After(100 * time.Millisecond):
		// expected: still blocked
	}

	// Release the first slot; second should unblock.
	release1()
	select {
	case <-acquired:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("second acquire did not unblock after release")
	}
}

func TestEndpointSemaphore_MultipleSlots(t *testing.T) {
	const slots = 4
	dir := t.TempDir()
	sem := newEndpointSemaphore(dir, slots)

	// Acquire all slots concurrently.
	releases := make([]func(), slots)
	for i := 0; i < slots; i++ {
		rel, err := sem.Acquire()
		require.NoError(t, err, "slot %d should be acquirable", i)
		releases[i] = rel
	}

	// (slots+1)th acquire should block.
	blocked := make(chan struct{})
	acquired := make(chan struct{})
	go func() {
		close(blocked)
		release, err := sem.Acquire()
		assert.NoError(t, err)
		close(acquired)
		release()
	}()

	<-blocked
	select {
	case <-acquired:
		t.Fatal("extra acquire should block when all slots held")
	case <-time.After(200 * time.Millisecond):
		// expected
	}

	// Release one slot; waiter should proceed.
	releases[0]()
	select {
	case <-acquired:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("blocked acquire did not unblock after release")
	}

	// Cleanup remaining.
	for i := 1; i < slots; i++ {
		releases[i]()
	}
}

func TestEndpointSemaphore_AutoDetectSlots(t *testing.T) {
	dir := t.TempDir()
	sem := newEndpointSemaphore(dir, 0) // 0 = auto = runtime.NumCPU()
	assert.Equal(t, runtime.NumCPU(), sem.slots)
}

func TestEndpointSemaphore_ConcurrentAcquireRelease(t *testing.T) {
	const slots = 4
	const goroutines = 20
	dir := t.TempDir()
	sem := newEndpointSemaphore(dir, slots)

	var maxConcurrent atomic.Int32
	var current atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := sem.Acquire()
			require.NoError(t, err)

			n := current.Add(1)
			// Track the high-water mark.
			for {
				old := maxConcurrent.Load()
				if n <= old || maxConcurrent.CompareAndSwap(old, n) {
					break
				}
			}

			// Simulate work.
			time.Sleep(10 * time.Millisecond)
			current.Add(-1)
			release()
		}()
	}

	wg.Wait()

	// The maximum concurrent acquisitions should not exceed the slot count.
	assert.LessOrEqual(t, int(maxConcurrent.Load()), slots,
		"concurrent acquisitions should not exceed slot count")
	assert.Greater(t, int(maxConcurrent.Load()), 0,
		"at least one goroutine should have acquired")
}

func TestEndpointSemaphore_ReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()
	sem := newEndpointSemaphore(dir, 2)

	// Acquire and release twice to verify reuse.
	for round := 0; round < 3; round++ {
		r1, err := sem.Acquire()
		require.NoError(t, err)
		r2, err := sem.Acquire()
		require.NoError(t, err)
		r1()
		r2()
	}
}
