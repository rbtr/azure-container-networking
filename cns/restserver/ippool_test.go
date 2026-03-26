package restserver

import (
	"fmt"
	"sync"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIPPool_PushPop(t *testing.T) {
	p := newIPPool()
	key := generateAssignedIPKey("nc1", cns.IPv4)

	p.Push(key, "ip-1")
	p.Push(key, "ip-2")
	p.Push(key, "ip-3")

	assert.Equal(t, 3, p.Len(key))
	assert.Equal(t, 3, p.Total())

	// Pop is LIFO
	id, ok := p.Pop(key)
	require.True(t, ok)
	assert.Equal(t, "ip-3", id)

	id, ok = p.Pop(key)
	require.True(t, ok)
	assert.Equal(t, "ip-2", id)

	id, ok = p.Pop(key)
	require.True(t, ok)
	assert.Equal(t, "ip-1", id)

	// Empty
	_, ok = p.Pop(key)
	assert.False(t, ok)
	assert.Equal(t, 0, p.Len(key))
}

func TestIPPool_Remove(t *testing.T) {
	p := newIPPool()
	key := generateAssignedIPKey("nc1", cns.IPv4)

	p.Push(key, "ip-1")
	p.Push(key, "ip-2")
	p.Push(key, "ip-3")

	// Remove middle element
	p.Remove("ip-2")
	assert.Equal(t, 2, p.Len(key))

	id, ok := p.Pop(key)
	require.True(t, ok)
	assert.Equal(t, "ip-3", id)

	id, ok = p.Pop(key)
	require.True(t, ok)
	assert.Equal(t, "ip-1", id)
}

func TestIPPool_RemoveNonexistent(t *testing.T) {
	p := newIPPool()
	key := generateAssignedIPKey("nc1", cns.IPv4)
	p.Push(key, "ip-1")

	// Removing a non-existent ID is a no-op
	p.Remove("ip-999")
	assert.Equal(t, 1, p.Len(key))
}

func TestIPPool_MultipleKeys(t *testing.T) {
	p := newIPPool()
	k4 := generateAssignedIPKey("nc1", cns.IPv4)
	k6 := generateAssignedIPKey("nc1", cns.IPv6)

	p.Push(k4, "v4-1")
	p.Push(k4, "v4-2")
	p.Push(k6, "v6-1")

	assert.Equal(t, 2, p.Len(k4))
	assert.Equal(t, 1, p.Len(k6))
	assert.Equal(t, 3, p.Total())

	id, ok := p.Pop(k6)
	require.True(t, ok)
	assert.Equal(t, "v6-1", id)

	// k4 unaffected
	assert.Equal(t, 2, p.Len(k4))
}

func TestIPPool_PopN(t *testing.T) {
	p := newIPPool()
	key := generateAssignedIPKey("nc1", cns.IPv4)

	p.Push(key, "ip-1")
	p.Push(key, "ip-2")
	p.Push(key, "ip-3")

	ids := p.PopN(key, 2)
	assert.Equal(t, []string{"ip-2", "ip-3"}, ids)
	assert.Equal(t, 1, p.Len(key))

	// PopN more than available
	ids = p.PopN(key, 5)
	assert.Equal(t, []string{"ip-1"}, ids)
	assert.Equal(t, 0, p.Len(key))

	// PopN on empty
	ids = p.PopN(key, 1)
	assert.Nil(t, ids)
}

func TestIPPool_PushAll(t *testing.T) {
	p := newIPPool()
	key := generateAssignedIPKey("nc1", cns.IPv4)

	p.PushAll(key, []string{"ip-1", "ip-2", "ip-3"})
	assert.Equal(t, 3, p.Len(key))

	id, _ := p.Pop(key)
	assert.Equal(t, "ip-3", id)
}

func TestIPPoolKeyFromIP(t *testing.T) {
	v4 := cns.IPConfigurationStatus{NCID: "nc1", IPAddress: "10.0.0.1"}
	assert.Equal(t, generateAssignedIPKey("nc1", cns.IPv4), ipPoolKeyFromIP(v4))

	v6 := cns.IPConfigurationStatus{NCID: "nc2", IPAddress: "fd00::1"}
	assert.Equal(t, generateAssignedIPKey("nc2", cns.IPv6), ipPoolKeyFromIP(v6))
}

func BenchmarkIPPool_Pop(b *testing.B) {
	key := generateAssignedIPKey("nc1", cns.IPv4)
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		p := newIPPool()
		for i := range 256 {
			p.Push(key, fmt.Sprintf("ip-%d", i))
		}
		b.StartTimer()
		p.Pop(key)
	}
}

// BenchmarkMapScan_MostAssigned simulates the realistic O(n) scan where
// 240 of 256 IPs are Assigned and only 16 are Available. The scan must
// iterate past many non-Available IPs to find one. This models the
// contention scenario observed in production (pod #30+ in a 50-pod burst).
func BenchmarkMapScan_MostAssigned(b *testing.B) {
	m := make(map[string]cns.IPConfigurationStatus, 256)
	for i := range 256 {
		ip := cns.IPConfigurationStatus{
			ID:        fmt.Sprintf("ip-%d", i),
			IPAddress: fmt.Sprintf("10.0.%d.%d", i/256, i%256),
			NCID:      "nc1",
		}
		if i < 240 {
			ip.SetState(types.Assigned)
		} else {
			ip.SetState(types.Available)
		}
		m[ip.ID] = ip
	}
	b.ResetTimer()
	for range b.N {
		for _, ip := range m { //nolint:gocritic // benchmarking the old approach
			if ip.GetState() == types.Available {
				break
			}
		}
	}
}

// BenchmarkPoolPop_MostAssigned shows O(1) pop regardless of how many IPs
// are assigned — the pool only tracks Available IPs.
func BenchmarkPoolPop_MostAssigned(b *testing.B) {
	key := generateAssignedIPKey("nc1", cns.IPv4)
	p := newIPPool()
	// Only 16 Available IPs in pool (240 are Assigned, not in pool)
	for i := 240; i < 256; i++ {
		p.Push(key, fmt.Sprintf("ip-%d", i))
	}
	b.ResetTimer()
	for range b.N {
		// Pop and immediately push back to keep pool stable
		if id, ok := p.Pop(key); ok {
			p.Push(key, id)
		}
	}
}

// BenchmarkConcurrentAssign_MapScan simulates 50 concurrent goroutines competing
// for a write lock, each performing an O(n) map scan. This is the contention
// pattern observed in production that causes 340-571ms tail latency.
func BenchmarkConcurrentAssign_MapScan(b *testing.B) {
	var mu sync.RWMutex
	m := make(map[string]cns.IPConfigurationStatus, 256)
	for i := range 256 {
		ip := cns.IPConfigurationStatus{
			ID:        fmt.Sprintf("ip-%d", i),
			IPAddress: fmt.Sprintf("10.0.%d.%d", i/256, i%256),
			NCID:      "nc1",
		}
		if i < 200 {
			ip.SetState(types.Assigned)
		} else {
			ip.SetState(types.Available)
		}
		m[ip.ID] = ip
	}

	b.SetParallelism(50)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			for _, ip := range m { //nolint:gocritic // benchmarking old approach
				if ip.GetState() == types.Available {
					break
				}
			}
			mu.Unlock()
		}
	})
}

// BenchmarkConcurrentAssign_Pool simulates 50 concurrent goroutines competing
// for a write lock, each performing an O(1) pool pop. Under contention, the
// total serialized time drops from ~N×O(n) to ~N×O(1).
func BenchmarkConcurrentAssign_Pool(b *testing.B) {
	var mu sync.RWMutex
	key := generateAssignedIPKey("nc1", cns.IPv4)
	p := newIPPool()
	for i := 200; i < 256; i++ {
		p.Push(key, fmt.Sprintf("ip-%d", i))
	}

	b.SetParallelism(50)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			mu.Lock()
			if id, ok := p.Pop(key); ok {
				p.Push(key, id)
			}
			mu.Unlock()
		}
	})
}
