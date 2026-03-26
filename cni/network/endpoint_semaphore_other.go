//go:build !linux

package network

// endpointSemaphore is a no-op on non-Linux platforms.
type endpointSemaphore struct{}

func newEndpointSemaphore(_ string, _ int) *endpointSemaphore {
	return &endpointSemaphore{}
}

func (s *endpointSemaphore) Acquire() (func(), error) {
	return func() {}, nil
}
