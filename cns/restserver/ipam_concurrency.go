// Copyright Microsoft. All rights reserved.
// MIT License

package restserver

import (
	"context"
	"time"

	"github.com/Azure/azure-container-networking/cns/logger"
)

// ipamSemaphore limits the number of concurrent IPAM request/release
// operations. When many pods start simultaneously, each IP assignment
// triggers CNI network setup (veth creation, route programming) that
// contends on the kernel's single RTNL lock. Limiting concurrency here
// bounds the queueing depth and prevents tail-latency blowup.
//
// A zero-capacity semaphore (nil channel) disables limiting.
type ipamSemaphore struct {
	sem chan struct{}
}

func newIPAMSemaphore(maxConcurrent int) *ipamSemaphore {
	if maxConcurrent <= 0 {
		return &ipamSemaphore{} // disabled
	}
	return &ipamSemaphore{sem: make(chan struct{}, maxConcurrent)}
}

// Acquire blocks until a slot is available or ctx expires.
// Returns a release function that must be called when the request
// completes, and an error if the context was cancelled while waiting.
func (s *ipamSemaphore) Acquire(ctx context.Context) (func(), error) {
	if s.sem == nil {
		return func() {}, nil // no-op when disabled
	}

	ipamConcurrencyQueueDepth.Inc()
	start := time.Now()

	select {
	case s.sem <- struct{}{}:
		ipamConcurrencyQueueDepth.Dec()
		ipamConcurrencyWaitTime.Observe(time.Since(start).Seconds())
		return func() { <-s.sem }, nil

	case <-ctx.Done():
		ipamConcurrencyQueueDepth.Dec()
		ipamConcurrencyWaitTime.Observe(time.Since(start).Seconds())
		logger.Errorf("[ipamSemaphore] request context expired after %v wait", time.Since(start))
		return nil, ctx.Err()
	}
}
