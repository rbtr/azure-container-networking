package store

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Azure/azure-container-networking/processlock"
	"go.uber.org/zap"
)

// EndpointInfo mirrors cns/restserver.EndpointInfo for benchmark realism.
type benchEndpointInfo struct {
	PodName       string
	PodNamespace  string
	IfnameToIPMap map[string]*benchIPInfo
}

// IPInfo mirrors cns/restserver.IPInfo for benchmark realism.
type benchIPInfo struct {
	IPv4               []net.IPNet
	IPv6               []net.IPNet `json:",omitempty"`
	HnsEndpointID      string      `json:",omitempty"`
	HnsNetworkID       string      `json:",omitempty"`
	HostVethName       string      `json:",omitempty"`
	MacAddress         string      `json:",omitempty"`
	NetworkContainerID string      `json:",omitempty"`
	NICType            string
}

// generateEndpointState creates a realistic endpoint state map with n endpoints.
// Each endpoint has 1-2 interfaces, each with 1 IPv4 and optionally 1 IPv6 address.
func generateEndpointState(n int) map[string]*benchEndpointInfo {
	state := make(map[string]*benchEndpointInfo, n)
	for i := 0; i < n; i++ {
		containerID := fmt.Sprintf("container-%06d-abcd-efgh-ijkl-%012d", i, i)
		ipv4 := net.IPNet{
			IP:   net.IPv4(10, byte(244+i/65536), byte((i/256)%256), byte(i%256)),
			Mask: net.CIDRMask(32, 32),
		}
		ipInfo := &benchIPInfo{
			IPv4:               []net.IPNet{ipv4},
			HostVethName:       fmt.Sprintf("veth%08x", i),
			NetworkContainerID: fmt.Sprintf("nc-swift-%04d", i%10),
			NICType:            "InfraNIC",
		}
		// ~30% of endpoints have dual-stack.
		if i%3 == 0 {
			ipv6 := net.IPNet{
				IP:   net.ParseIP(fmt.Sprintf("fd00::%04x", i)),
				Mask: net.CIDRMask(128, 128),
			}
			ipInfo.IPv6 = []net.IPNet{ipv6}
		}
		state[containerID] = &benchEndpointInfo{
			PodName:      fmt.Sprintf("pod-%d", i),
			PodNamespace: "default",
			IfnameToIPMap: map[string]*benchIPInfo{
				"eth0": ipInfo,
			},
		}
	}
	return state
}

type storeFactory struct {
	name    string
	create  func(dir string) (KeyValueStore, error)
	cleanup func(s KeyValueStore)
}

func storeFactories() []storeFactory {
	return []storeFactory{
		{
			name: "JSON",
			create: func(dir string) (KeyValueStore, error) {
				fp := filepath.Join(dir, "test.json")
				lock := processlock.NewMockFileLock(false)
				return NewJsonFileStore(fp, lock, zap.NewNop())
			},
			cleanup: func(s KeyValueStore) { s.Remove() },
		},
		{
			name: "BoltDB",
			create: func(dir string) (KeyValueStore, error) {
				fp := filepath.Join(dir, "test.db")
				return NewBoltStore(fp)
			},
			cleanup: func(s KeyValueStore) {
				if c, ok := s.(interface{ Close() error }); ok {
					c.Close()
				}
				s.Remove()
			},
		},
		{
			name: "SQLite",
			create: func(dir string) (KeyValueStore, error) {
				fp := filepath.Join(dir, "test.sqlite")
				return NewSQLiteStore(fp)
			},
			cleanup: func(s KeyValueStore) {
				if c, ok := s.(interface{ Close() error }); ok {
					c.Close()
				}
				s.Remove()
			},
		},
	}
}

// BenchmarkStoreWrite benchmarks writing a full endpoint state blob to each store backend.
// This simulates the hot path in updateEndpointState/removeEndpointState.
func BenchmarkStoreWrite(b *testing.B) {
	sizes := []int{50, 250, 500}
	for _, sf := range storeFactories() {
		for _, n := range sizes {
			state := generateEndpointState(n)
			b.Run(fmt.Sprintf("%s/endpoints=%d", sf.name, n), func(b *testing.B) {
				dir := b.TempDir()
				s, err := sf.create(dir)
				if err != nil {
					b.Fatal(err)
				}
				defer sf.cleanup(s)

				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := s.Write("Endpoints", state); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkStoreRead benchmarks reading a full endpoint state blob from each store backend.
// This simulates restoreState at startup.
func BenchmarkStoreRead(b *testing.B) {
	sizes := []int{50, 250, 500}
	for _, sf := range storeFactories() {
		for _, n := range sizes {
			state := generateEndpointState(n)
			b.Run(fmt.Sprintf("%s/endpoints=%d", sf.name, n), func(b *testing.B) {
				dir := b.TempDir()
				s, err := sf.create(dir)
				if err != nil {
					b.Fatal(err)
				}
				defer sf.cleanup(s)

				// Seed the store with data.
				if err := s.Write("Endpoints", state); err != nil {
					b.Fatal(err)
				}

				// For JSON store, force re-read from disk each iteration.
				b.ResetTimer()
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					var result map[string]*benchEndpointInfo
					if err := s.Read("Endpoints", &result); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkStoreConcurrentWrite benchmarks concurrent writes from multiple goroutines.
// This simulates multiple pods being scheduled simultaneously, each triggering an
// endpoint state update that acquires the lock and writes to the store.
func BenchmarkStoreConcurrentWrite(b *testing.B) {
	concurrencies := []int{4, 16, 64}
	for _, sf := range storeFactories() {
		for _, conc := range concurrencies {
			b.Run(fmt.Sprintf("%s/goroutines=%d", sf.name, conc), func(b *testing.B) {
				dir := b.TempDir()
				s, err := sf.create(dir)
				if err != nil {
					b.Fatal(err)
				}
				defer sf.cleanup(s)

				// Start with a base state of 250 endpoints.
				baseState := generateEndpointState(250)
				if err := s.Write("Endpoints", baseState); err != nil {
					b.Fatal(err)
				}

				b.ResetTimer()
				b.ReportAllocs()

				b.RunParallel(func(pb *testing.PB) {
					// Each goroutine writes the full state (simulating the current behavior
					// where each updateEndpointState call writes the entire map).
					for pb.Next() {
						if err := s.Write("Endpoints", baseState); err != nil {
							b.Fatal(err)
						}
					}
				})
			})
		}
	}
}

// BenchmarkStoreWriteRead benchmarks interleaved reads and writes, simulating
// a realistic mix of IP allocations (writes) and status queries (reads).
func BenchmarkStoreWriteRead(b *testing.B) {
	for _, sf := range storeFactories() {
		b.Run(sf.name, func(b *testing.B) {
			dir := b.TempDir()
			s, err := sf.create(dir)
			if err != nil {
				b.Fatal(err)
			}
			defer sf.cleanup(s)

			state := generateEndpointState(250)
			if err := s.Write("Endpoints", state); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					// 80% reads, 20% writes (typical IPAM workload: many status checks, fewer allocations).
					if rand.Intn(5) == 0 { //nolint:gosec // benchmark, not crypto
						if err := s.Write("Endpoints", state); err != nil {
							b.Fatal(err)
						}
					} else {
						var result map[string]*benchEndpointInfo
						if err := s.Read("Endpoints", &result); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		})
	}
}

// BenchmarkStoreIncrementalWrite benchmarks the pattern of adding one endpoint at a time
// to a growing state, which is what happens during pod scheduling bursts.
func BenchmarkStoreIncrementalWrite(b *testing.B) {
	for _, sf := range storeFactories() {
		b.Run(sf.name, func(b *testing.B) {
			dir := b.TempDir()
			s, err := sf.create(dir)
			if err != nil {
				b.Fatal(err)
			}
			defer sf.cleanup(s)

			state := generateEndpointState(0)

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				// Add one more endpoint to the state, then write the whole thing.
				// This simulates the O(n) growth problem with the JSON store.
				containerID := fmt.Sprintf("container-inc-%06d", i)
				state[containerID] = &benchEndpointInfo{
					PodName:      fmt.Sprintf("pod-%d", i),
					PodNamespace: "default",
					IfnameToIPMap: map[string]*benchIPInfo{
						"eth0": {
							IPv4: []net.IPNet{{
								IP:   net.IPv4(10, 244, byte(i/256), byte(i%256)),
								Mask: net.CIDRMask(32, 32),
							}},
							NICType: "InfraNIC",
						},
					},
				}
				if err := s.Write("Endpoints", state); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestStoreBackendCorrectness verifies all backends produce identical results.
func TestStoreBackendCorrectness(t *testing.T) {
	for _, sf := range storeFactories() {
		t.Run(sf.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := sf.create(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer sf.cleanup(s)

			state := generateEndpointState(100)

			// Write.
			if err := s.Write("Endpoints", state); err != nil {
				t.Fatal("write failed:", err)
			}

			// Read back.
			var result map[string]*benchEndpointInfo
			if err := s.Read("Endpoints", &result); err != nil {
				t.Fatal("read failed:", err)
			}

			// Verify count.
			if len(result) != 100 {
				t.Fatalf("expected 100 endpoints, got %d", len(result))
			}

			// Spot-check a value.
			ep, ok := result["container-000042-abcd-efgh-ijkl-000000000042"]
			if !ok {
				t.Fatal("missing expected endpoint container-000042")
			}
			if ep.PodName != "pod-42" {
				t.Fatalf("expected pod-42, got %s", ep.PodName)
			}
			if len(ep.IfnameToIPMap["eth0"].IPv4) != 1 {
				t.Fatalf("expected 1 IPv4, got %d", len(ep.IfnameToIPMap["eth0"].IPv4))
			}

			// Verify KeyNotFound.
			var empty map[string]*benchEndpointInfo
			err = s.Read("nonexistent", &empty)
			if err != ErrKeyNotFound {
				t.Fatalf("expected ErrKeyNotFound, got %v", err)
			}

			// Test overwrite with additional entry.
			state["new-container"] = &benchEndpointInfo{PodName: "new-pod", PodNamespace: "kube-system"}
			if err := s.Write("Endpoints", state); err != nil {
				t.Fatal("overwrite failed:", err)
			}
			var result2 map[string]*benchEndpointInfo
			if err := s.Read("Endpoints", &result2); err != nil {
				t.Fatal("read after overwrite failed:", err)
			}
			if len(result2) != 101 {
				t.Fatalf("expected 101 endpoints after overwrite, got %d", len(result2))
			}

			// Test Exists.
			if !s.Exists() {
				t.Fatal("store should exist")
			}
		})
	}
}

// TestStoreBackendConcurrency verifies backends handle concurrent access correctly.
func TestStoreBackendConcurrency(t *testing.T) {
	for _, sf := range storeFactories() {
		t.Run(sf.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := sf.create(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer sf.cleanup(s)

			state := generateEndpointState(50)
			if err := s.Write("Endpoints", state); err != nil {
				t.Fatal(err)
			}

			var wg sync.WaitGroup
			errs := make(chan error, 200)

			// 10 concurrent writers.
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 10; j++ {
						if err := s.Write("Endpoints", state); err != nil {
							errs <- err
						}
					}
				}()
			}

			// 10 concurrent readers.
			for i := 0; i < 10; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 10; j++ {
						var result map[string]*benchEndpointInfo
						if err := s.Read("Endpoints", &result); err != nil {
							errs <- err
						}
					}
				}()
			}

			wg.Wait()
			close(errs)

			for err := range errs {
				t.Errorf("concurrent operation failed: %v", err)
			}
		})
	}
}

// TestMain handles cleanup for SQLite temp files.
func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(code)
}
