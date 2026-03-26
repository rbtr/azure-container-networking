//go:build linux

package network

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"go.uber.org/zap"
)

const (
	defaultSemDir   = "/var/run/azure-vnet/endpoint-sem"
	defaultMaxSlots = 0 // 0 = auto-detect (runtime.NumCPU())
)

// endpointSemaphore is a cross-process counting semaphore implemented
// using flock(). It limits concurrent endpoint creation across all CNI
// processes on a host to reduce kernel RTNL lock contention.
type endpointSemaphore struct {
	dir   string
	slots int
}

func newEndpointSemaphore(dir string, maxSlots int) *endpointSemaphore {
	if maxSlots <= 0 {
		maxSlots = runtime.NumCPU()
	}
	if dir == "" {
		dir = defaultSemDir
	}
	sem := &endpointSemaphore{
		dir:   dir,
		slots: maxSlots,
	}
	// Best-effort create the semaphore directory.
	os.MkdirAll(sem.dir, 0o755) //nolint:errcheck // best-effort
	return sem
}

// Acquire tries to grab one of the N flock slots. Returns a release
// function that MUST be called (typically via defer).
func (s *endpointSemaphore) Acquire() (release func(), err error) {
	// Try each slot with LOCK_NB first (non-blocking scan).
	for i := 0; i < s.slots; i++ {
		path := filepath.Join(s.dir, fmt.Sprintf("slot-%d.lock", i))
		f, openErr := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if openErr != nil {
			continue
		}
		if flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flockErr == nil {
			logger.Info("Acquired endpoint semaphore slot",
				zap.Int("slot", i),
				zap.Int("total", s.slots))
			return func() {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock
				f.Close()
			}, nil
		}
		f.Close() // slot is taken, try next
	}

	// All slots taken — block on slot-0 (FIFO fairness).
	logger.Info("All endpoint semaphore slots busy, blocking",
		zap.Int("slots", s.slots))
	start := time.Now()
	path := filepath.Join(s.dir, "slot-0.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {}, fmt.Errorf("failed to open semaphore slot: %w", err)
	}
	// Blocking flock.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}, fmt.Errorf("failed to acquire semaphore: %w", err)
	}
	logger.Info("Acquired endpoint semaphore after wait",
		zap.Duration("wait", time.Since(start)),
		zap.Int("slots", s.slots))
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock
		f.Close()
	}, nil
}
