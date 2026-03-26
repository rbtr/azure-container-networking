// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/store"
)

// makeEndpointMap builds a realistic in-memory endpoint state map of the
// given size, as used by the old JSON store path.
func makeEndpointMap(n int) map[string]endpointInfoLike {
	m := make(map[string]endpointInfoLike, n)
	for i := 0; i < n; i++ {
		cid := fmt.Sprintf("%064x", i)
		m[cid] = endpointInfoLike{
			PodName:      fmt.Sprintf("pod-%d", i),
			PodNamespace: "default",
			IfnameToIPMap: map[string]*ipInfoLike{
				"eth0": {
					IPv4: []net.IPNet{
						{IP: net.IPv4(10, 0, byte(i>>8), byte(i)), Mask: net.IPv4Mask(255, 255, 255, 0)},
					},
					NICType: cns.InfraNIC,
				},
			},
		}
	}
	return m
}

// endpointInfoLike mirrors restserver.EndpointInfo for JSON serialisation
// (avoids import cycle with restserver package).
type endpointInfoLike struct {
	PodName       string
	PodNamespace  string
	IfnameToIPMap map[string]*ipInfoLike
}

type ipInfoLike struct {
	IPv4               []net.IPNet
	IPv6               []net.IPNet `json:",omitempty"`
	HnsEndpointID      string      `json:",omitempty"`
	HnsNetworkID       string      `json:",omitempty"`
	HostVethName       string      `json:",omitempty"`
	MacAddress         string      `json:",omitempty"`
	NetworkContainerID string      `json:",omitempty"`
	NICType            cns.NICType
}

// jsonWholeMapWrite simulates the old JSON store: marshal the entire map
// and write to disk atomically.
func jsonWholeMapWrite(path string, state map[string]endpointInfoLike) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// BenchmarkJSON_WholeMapWrite measures the old-style full-map JSON write.
func BenchmarkJSON_WholeMapWrite(b *testing.B) {
	for _, size := range []int{50, 100, 250, 500} {
		state := makeEndpointMap(size)
		path := b.TempDir() + "/endpoints.json"
		b.Run(fmt.Sprintf("endpoints=%d", size), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := jsonWholeMapWrite(path, state); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBolt_PutEndpoint measures a single per-record bolt write.
func BenchmarkBolt_PutEndpoint(b *testing.B) {
	for _, size := range []int{50, 100, 250, 500} {
		b.Run(fmt.Sprintf("preloaded=%d", size), func(b *testing.B) {
			path := b.TempDir() + "/endpoints.bolt.db"
			s, err := store.OpenEndpointStore(path, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()

			// Pre-populate
			for i := 0; i < size; i++ {
				cid := fmt.Sprintf("%064x", i)
				ep := store.EndpointRecord{
					PodName:      fmt.Sprintf("pod-%d", i),
					PodNamespace: "default",
					IfnameToIPMap: map[string]*store.IPInfoRecord{
						"eth0": {
							IPv4:    []net.IPNet{{IP: net.IPv4(10, 0, byte(i>>8), byte(i)), Mask: net.IPv4Mask(255, 255, 255, 0)}},
							NICType: cns.InfraNIC,
						},
					},
				}
				if err := s.PutEndpoint(ctx, cid, ep); err != nil {
					b.Fatal(err)
				}
			}

			// Benchmark: write a single endpoint (simulates IPAM hot path)
			newEP := store.EndpointRecord{
				PodName:      "bench-pod",
				PodNamespace: "default",
				IfnameToIPMap: map[string]*store.IPInfoRecord{
					"eth0": {
						IPv4:    []net.IPNet{{IP: net.IPv4(10, 1, 0, 1), Mask: net.IPv4Mask(255, 255, 255, 0)}},
						NICType: cns.InfraNIC,
					},
				},
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cid := fmt.Sprintf("bench-%064x", i)
				if err := s.PutEndpoint(ctx, cid, newEP); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBolt_DeleteEndpoint measures per-record bolt delete.
func BenchmarkBolt_DeleteEndpoint(b *testing.B) {
	path := b.TempDir() + "/endpoints.bolt.db"
	s, err := store.OpenEndpointStore(path, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	ep := store.EndpointRecord{PodName: "p", PodNamespace: "ns"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cid := fmt.Sprintf("del-%064x", i)
		// put then delete to simulate IPAM release
		_ = s.PutEndpoint(ctx, cid, ep)
		if err := s.DeleteEndpoint(ctx, cid); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBolt_ConcurrentPutEndpoint measures per-record writes under
// concurrent access (simulating multiple IPAM requests).
func BenchmarkBolt_ConcurrentPutEndpoint(b *testing.B) {
	for _, goroutines := range []int{4, 8, 16, 32} {
		b.Run(fmt.Sprintf("goroutines=%d", goroutines), func(b *testing.B) {
			path := b.TempDir() + "/endpoints.bolt.db"
			s, err := store.OpenEndpointStore(path, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()

			ep := store.EndpointRecord{
				PodName:      "bench-pod",
				PodNamespace: "default",
				IfnameToIPMap: map[string]*store.IPInfoRecord{
					"eth0": {
						IPv4:    []net.IPNet{{IP: net.IPv4(10, 1, 0, 1), Mask: net.IPv4Mask(255, 255, 255, 0)}},
						NICType: cns.InfraNIC,
					},
				},
			}

			b.ResetTimer()
			b.SetParallelism(goroutines)
			b.RunParallel(func(pb *testing.PB) {
				var i int
				for pb.Next() {
					cid := fmt.Sprintf("par-%d-%d", goroutines, i)
					_ = s.PutEndpoint(ctx, cid, ep)
					i++
				}
			})
		})
	}
}

// BenchmarkJSON_ConcurrentWholeMapWrite measures old-style writes under
// concurrent access (would need external mutex in practice).
func BenchmarkJSON_ConcurrentWholeMapWrite(b *testing.B) {
	for _, goroutines := range []int{4, 8, 16, 32} {
		b.Run(fmt.Sprintf("goroutines=%d", goroutines), func(b *testing.B) {
			state := makeEndpointMap(250)
			path := b.TempDir() + "/endpoints.json"
			var mu sync.Mutex

			b.ResetTimer()
			b.SetParallelism(goroutines)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					mu.Lock()
					_ = jsonWholeMapWrite(path, state)
					mu.Unlock()
				}
			})
		})
	}
}

// BenchmarkJSON_vs_Bolt_AddEndpoint is the key comparison: add a single
// endpoint to existing state.
//
//   - JSON: must re-serialize the entire map + write entire file
//   - Bolt: writes only the one new record
func BenchmarkJSON_vs_Bolt_AddEndpoint(b *testing.B) {
	for _, size := range []int{50, 100, 250, 500} {
		b.Run(fmt.Sprintf("existing=%d/json", size), func(b *testing.B) {
			state := makeEndpointMap(size)
			path := b.TempDir() + "/endpoints.json"
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Simulate adding one endpoint and writing entire state
				cid := fmt.Sprintf("new-%064x", i)
				state[cid] = endpointInfoLike{PodName: "new-pod", PodNamespace: "default"}
				if err := jsonWholeMapWrite(path, state); err != nil {
					b.Fatal(err)
				}
				delete(state, cid) // clean up so map doesn't grow unbounded
			}
		})

		b.Run(fmt.Sprintf("existing=%d/bolt", size), func(b *testing.B) {
			path := b.TempDir() + "/endpoints.bolt.db"
			s, err := store.OpenEndpointStore(path, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			ctx := context.Background()

			// Pre-populate
			for i := 0; i < size; i++ {
				cid := fmt.Sprintf("%064x", i)
				ep := store.EndpointRecord{
					PodName:      fmt.Sprintf("pod-%d", i),
					PodNamespace: "default",
					IfnameToIPMap: map[string]*store.IPInfoRecord{
						"eth0": {
							IPv4:    []net.IPNet{{IP: net.IPv4(10, 0, byte(i>>8), byte(i)), Mask: net.IPv4Mask(255, 255, 255, 0)}},
							NICType: cns.InfraNIC,
						},
					},
				}
				if err := s.PutEndpoint(ctx, cid, ep); err != nil {
					b.Fatal(err)
				}
			}

			newEP := store.EndpointRecord{PodName: "new-pod", PodNamespace: "default"}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cid := fmt.Sprintf("new-%064x", i)
				if err := s.PutEndpoint(ctx, cid, newEP); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
