package restserver

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	cnsstore "github.com/Azure/azure-container-networking/cns/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTestEndpointStore(t *testing.T) *cnsstore.EndpointBoltStore {
	t.Helper()
	s, err := cnsstore.OpenEndpointStore(t.TempDir()+"/test-ep.bolt.db", nil)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEndpointWriter_PutAndClose(t *testing.T) {
	s := openTestEndpointStore(t)
	w := newEndpointWriter(s)

	info := &EndpointInfo{
		PodName:      "pod1",
		PodNamespace: "ns1",
		IfnameToIPMap: map[string]*IPInfo{
			"eth0": {IPv4: []net.IPNet{{IP: net.IPv4(10, 0, 0, 1), Mask: net.IPv4Mask(255, 255, 255, 0)}}},
		},
	}

	w.PutEndpoint("abc123", info)
	w.Close() // blocks until write completes

	// Verify the record was persisted
	rec, err := s.GetEndpoint(context.Background(), "abc123")
	require.NoError(t, err)
	assert.Equal(t, "pod1", rec.PodName)
	assert.Equal(t, "ns1", rec.PodNamespace)
	assert.Len(t, rec.IfnameToIPMap["eth0"].IPv4, 1)
}

func TestEndpointWriter_DeleteAndClose(t *testing.T) {
	s := openTestEndpointStore(t)

	// Pre-populate
	err := s.PutEndpoint(context.Background(), "ctr1", cnsstore.EndpointRecord{PodName: "pod1"})
	require.NoError(t, err)

	w := newEndpointWriter(s)
	w.DeleteEndpoint("ctr1")
	w.Close()

	_, err = s.GetEndpoint(context.Background(), "ctr1")
	assert.ErrorIs(t, err, cnsstore.ErrNotFound)
}

func TestEndpointWriter_DeepCopiesState(t *testing.T) {
	s := openTestEndpointStore(t)
	w := newEndpointWriter(s)

	info := &EndpointInfo{
		PodName:       "pod1",
		PodNamespace:  "ns1",
		IfnameToIPMap: map[string]*IPInfo{"eth0": {IPv4: []net.IPNet{{IP: net.IPv4(10, 0, 0, 1), Mask: net.IPv4Mask(255, 255, 255, 0)}}}},
	}

	w.PutEndpoint("ctr1", info)

	// Mutate the original after PutEndpoint returns
	info.PodName = "MUTATED"

	w.Close()

	rec, err := s.GetEndpoint(context.Background(), "ctr1")
	require.NoError(t, err)
	assert.Equal(t, "pod1", rec.PodName, "persisted record should not reflect mutation")
}

func TestIPAMSemaphore_Disabled(t *testing.T) {
	s := newIPAMSemaphore(0)
	release, err := s.Acquire(context.Background())
	require.NoError(t, err)
	release() // should be a no-op
}

func TestIPAMSemaphore_LimitsConcurrency(t *testing.T) {
	const maxConcurrent = 3
	s := newIPAMSemaphore(maxConcurrent)

	var (
		mu     sync.Mutex
		active int
		maxSeen int
		wg     sync.WaitGroup
	)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.Acquire(context.Background())
			if err != nil {
				return
			}
			defer release()

			mu.Lock()
			active++
			if active > maxSeen {
				maxSeen = active
			}
			mu.Unlock()

			time.Sleep(5 * time.Millisecond) // simulate work

			mu.Lock()
			active--
			mu.Unlock()
		}()
	}

	wg.Wait()
	assert.LessOrEqual(t, maxSeen, maxConcurrent, "concurrency exceeded limit")
	assert.Greater(t, maxSeen, 0, "semaphore should have allowed some concurrency")
}

func TestIPAMSemaphore_ContextCancellation(t *testing.T) {
	s := newIPAMSemaphore(1)

	// Fill the single slot
	release, err := s.Acquire(context.Background())
	require.NoError(t, err)

	// Try to acquire with a cancelled context — should fail
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = s.Acquire(ctx)
	assert.ErrorIs(t, err, context.Canceled)

	release() // free the slot
}
