// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// openTestNCStore creates a temporary boltdb NC store and registers cleanup.
func openTestNCStore(t *testing.T) *store.NCBoltStore {
	t.Helper()
	path := t.TempDir() + "/test-cns.db"
	s, err := store.OpenNCStore(path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// openTestEndpointStore creates a temporary boltdb endpoint store and registers cleanup.
func openTestEndpointStore(t *testing.T) *store.EndpointBoltStore {
	t.Helper()
	path := t.TempDir() + "/test-endpoints.db"
	s, err := store.OpenEndpointStore(path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// ---- StoreMeta ----

func TestBoltNCStore_PutGetMeta(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	now := time.Now().UTC().Truncate(time.Second) // binary encoding loses sub-second
	in := store.StoreMeta{
		OrchestratorType: "KubernetesCRD",
		NodeID:           "node-abc",
		Location:         "eastus",
		NetworkType:      "overlay",
		Initialized:      true,
		TimeStamp:        now,
	}
	require.NoError(t, s.PutMeta(ctx, in))

	got, err := s.GetMeta(ctx)
	require.NoError(t, err)
	assert.Equal(t, store.SchemaVersion, got.Version)
	assert.Equal(t, in.OrchestratorType, got.OrchestratorType)
	assert.Equal(t, in.NodeID, got.NodeID)
	assert.Equal(t, in.Location, got.Location)
	assert.Equal(t, in.NetworkType, got.NetworkType)
	assert.Equal(t, in.Initialized, got.Initialized)
	assert.Equal(t, in.TimeStamp, got.TimeStamp)
}

func TestBoltNCStore_SchemaVersion(t *testing.T) {
	path := t.TempDir() + "/schema.db"
	s, err := store.OpenNCStore(path, nil)
	require.NoError(t, err)

	meta, err := s.GetMeta(context.Background())
	require.NoError(t, err)
	assert.Equal(t, store.SchemaVersion, meta.Version)
	s.Close()

	// Re-open the same file; schema version must match.
	s2, err := store.OpenNCStore(path, nil)
	require.NoError(t, err)
	defer s2.Close()

	meta2, err := s2.GetMeta(context.Background())
	require.NoError(t, err)
	assert.Equal(t, store.SchemaVersion, meta2.Version)
}

// ---- Network Containers ----

func sampleNC(id string) store.NCRecord {
	return store.NCRecord{
		ID:                   id,
		VMVersion:            "1",
		HostVersion:          "2",
		VfpUpdateComplete:    true,
		HostPrimaryIP:        "10.0.0.4",
		Version:              "3",
		NetworkContainerType: "Docker",
		IPConfiguration: cns.IPConfiguration{
			IPSubnet: cns.IPSubnet{IPAddress: "192.168.0.0", PrefixLength: 24},
		},
	}
}

func TestBoltNCStore_PutGetNC(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	in := sampleNC("nc-1")
	require.NoError(t, s.PutNC(ctx, in))

	got, err := s.GetNC(ctx, "nc-1")
	require.NoError(t, err)
	assert.Equal(t, in, got)
}

func TestBoltNCStore_GetNC_NotFound(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	_, err := s.GetNC(ctx, "does-not-exist")
	require.Error(t, err)
}

func TestBoltNCStore_DeleteNC(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	require.NoError(t, s.PutNC(ctx, sampleNC("nc-2")))
	require.NoError(t, s.DeleteNC(ctx, "nc-2"))

	_, err := s.GetNC(ctx, "nc-2")
	require.Error(t, err)
}

func TestBoltNCStore_ListNCs(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	for _, id := range []string{"nc-a", "nc-b", "nc-c"} {
		require.NoError(t, s.PutNC(ctx, sampleNC(id)))
	}

	ncs, err := s.ListNCs(ctx)
	require.NoError(t, err)
	assert.Len(t, ncs, 3)
}

func TestBoltNCStore_ListNCs_Empty(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	ncs, err := s.ListNCs(ctx)
	require.NoError(t, err)
	assert.Empty(t, ncs)
}

// ---- Secondary IPs ----

func sampleIP(addr, ncID string) store.IPRecord {
	return store.IPRecord{IPAddress: addr, NCID: ncID, NCVersion: 42}
}

func TestBoltNCStore_PutGetIP(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	in := sampleIP("10.0.0.1", "nc-x")
	require.NoError(t, s.PutIP(ctx, in))

	got, err := s.GetIP(ctx, "10.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, in, got)
}

func TestBoltNCStore_GetIP_NotFound(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	_, err := s.GetIP(ctx, "1.2.3.4")
	require.Error(t, err)
}

func TestBoltNCStore_DeleteIP(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	require.NoError(t, s.PutIP(ctx, sampleIP("10.0.0.5", "nc-del")))
	require.NoError(t, s.DeleteIP(ctx, "10.0.0.5"))

	_, err := s.GetIP(ctx, "10.0.0.5")
	require.Error(t, err)
}

func TestBoltNCStore_DeleteIPsByNCID(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	// 5 IPs on nc-1, 3 IPs on nc-2.
	for i := 1; i <= 5; i++ {
		addr := net.IPv4(10, 0, 1, byte(i)).String()
		require.NoError(t, s.PutIP(ctx, sampleIP(addr, "nc-1")))
	}
	for i := 1; i <= 3; i++ {
		addr := net.IPv4(10, 0, 2, byte(i)).String()
		require.NoError(t, s.PutIP(ctx, sampleIP(addr, "nc-2")))
	}

	require.NoError(t, s.DeleteIPsByNCID(ctx, "nc-1"))

	ips, err := s.ListIPs(ctx)
	require.NoError(t, err)
	assert.Len(t, ips, 3)
	for _, ip := range ips {
		assert.Equal(t, "nc-2", ip.NCID)
	}
}

func TestBoltNCStore_ListIPs(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	for i := 1; i <= 10; i++ {
		addr := net.IPv4(192, 168, 0, byte(i)).String()
		require.NoError(t, s.PutIP(ctx, sampleIP(addr, "nc-list")))
	}

	ips, err := s.ListIPs(ctx)
	require.NoError(t, err)
	assert.Len(t, ips, 10)
}

// ---- Networks ----

func sampleNetwork(name string) store.NetworkRecord {
	return store.NetworkRecord{
		NetworkName: name,
		NicInfo: &store.NicInfoRecord{
			Subnet:    "10.0.0.0/24",
			Gateway:   "10.0.0.1",
			IsPrimary: true,
			PrimaryIP: "10.0.0.4",
		},
		Options: map[string]interface{}{"mode": "bridge"},
	}
}

func TestBoltNCStore_PutGetNetwork(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	in := sampleNetwork("net-1")
	require.NoError(t, s.PutNetwork(ctx, in))

	got, err := s.GetNetwork(ctx, "net-1")
	require.NoError(t, err)
	assert.Equal(t, in.NetworkName, got.NetworkName)
	assert.Equal(t, in.NicInfo, got.NicInfo)
}

func TestBoltNCStore_DeleteNetwork(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	require.NoError(t, s.PutNetwork(ctx, sampleNetwork("net-del")))
	require.NoError(t, s.DeleteNetwork(ctx, "net-del"))

	_, err := s.GetNetwork(ctx, "net-del")
	require.Error(t, err)
}

func TestBoltNCStore_ListNetworks(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	for _, name := range []string{"net-a", "net-b"} {
		require.NoError(t, s.PutNetwork(ctx, sampleNetwork(name)))
	}

	nets, err := s.ListNetworks(ctx)
	require.NoError(t, err)
	assert.Len(t, nets, 2)
}

// ---- Orchestrator context ----

func TestBoltNCStore_OrchestratorContext(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	key := `{"podName":"foo","podNamespace":"default"}`
	want := []string{"nc-1", "nc-2"}
	require.NoError(t, s.PutOrchestratorContext(ctx, key, want))

	got, err := s.GetOrchestratorContext(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestBoltNCStore_DeleteOrchestratorContext(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	key := "ctx-key"
	require.NoError(t, s.PutOrchestratorContext(ctx, key, []string{"nc-x"}))
	require.NoError(t, s.DeleteOrchestratorContext(ctx, key))

	_, err := s.GetOrchestratorContext(ctx, key)
	require.Error(t, err)
}

func TestBoltNCStore_ListOrchestratorContexts(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	require.NoError(t, s.PutOrchestratorContext(ctx, "k1", []string{"a"}))
	require.NoError(t, s.PutOrchestratorContext(ctx, "k2", []string{"b", "c"}))

	all, err := s.ListOrchestratorContexts(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
	assert.Equal(t, []string{"a"}, all["k1"])
	assert.Equal(t, []string{"b", "c"}, all["k2"])
}

func TestBoltNCStore_OrchestratorContext_EmptyList(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	key := "ctx-empty"
	require.NoError(t, s.PutOrchestratorContext(ctx, key, nil))

	got, err := s.GetOrchestratorContext(ctx, key)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// ---- PnP / MAC ----

func TestBoltNCStore_PnpMAC(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	require.NoError(t, s.PutPnpIDByMAC(ctx, "00:11:22:33:44:55", "pnp-abc"))

	got, err := s.GetPnpIDByMAC(ctx, "00:11:22:33:44:55")
	require.NoError(t, err)
	assert.Equal(t, "pnp-abc", got)

	all, err := s.ListPnpIDByMAC(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"00:11:22:33:44:55": "pnp-abc"}, all)
}

// ---- Endpoints ----

func sampleEndpoint(podName, ns string) store.EndpointRecord {
	ip := net.ParseIP("192.168.0.1")
	ep := store.EndpointRecord{
		PodName:      podName,
		PodNamespace: ns,
		IfnameToIPMap: map[string]*store.IPInfoRecord{
			"eth0": {
				IPv4: []net.IPNet{{IP: ip, Mask: net.CIDRMask(24, 32)}},
			},
		},
	}
	return ep
}

func TestBoltEndpointStore_PutGetEndpoint(t *testing.T) {
	ctx := context.Background()
	s := openTestEndpointStore(t)

	in := sampleEndpoint("my-pod", "default")
	require.NoError(t, s.PutEndpoint(ctx, "container-abc", in))

	got, err := s.GetEndpoint(ctx, "container-abc")
	require.NoError(t, err)
	assert.Equal(t, in.PodName, got.PodName)
	assert.Equal(t, in.PodNamespace, got.PodNamespace)
	require.Contains(t, got.IfnameToIPMap, "eth0")
	assert.Equal(t, in.IfnameToIPMap["eth0"].IPv4[0].String(),
		got.IfnameToIPMap["eth0"].IPv4[0].String())
}

func TestBoltEndpointStore_GetEndpoint_NotFound(t *testing.T) {
	ctx := context.Background()
	s := openTestEndpointStore(t)

	_, err := s.GetEndpoint(ctx, "no-such-container")
	require.Error(t, err)
}

func TestBoltEndpointStore_DeleteEndpoint(t *testing.T) {
	ctx := context.Background()
	s := openTestEndpointStore(t)

	require.NoError(t, s.PutEndpoint(ctx, "cid-del", sampleEndpoint("p", "ns")))
	require.NoError(t, s.DeleteEndpoint(ctx, "cid-del"))

	_, err := s.GetEndpoint(ctx, "cid-del")
	require.Error(t, err)
}

func TestBoltEndpointStore_ListEndpoints(t *testing.T) {
	ctx := context.Background()
	s := openTestEndpointStore(t)

	for i, id := range []string{"c1", "c2", "c3"} {
		ep := sampleEndpoint("pod-"+id, "default")
		// Give each endpoint a distinct IP to avoid map key collisions in the
		// IPNet comparison.
		ep.IfnameToIPMap["eth0"].IPv4[0].IP = net.ParseIP("10.0.0." + string(rune('1'+i)))
		require.NoError(t, s.PutEndpoint(ctx, id, ep))
	}

	eps, err := s.ListEndpoints(ctx)
	require.NoError(t, err)
	assert.Len(t, eps, 3)
}

// ---- Persistence across close/reopen ----

func TestBoltStore_ReopenPersistence(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/persist.db"

	{
		s, err := store.OpenNCStore(path, nil)
		require.NoError(t, err)
		require.NoError(t, s.PutNC(ctx, sampleNC("nc-persist")))
		require.NoError(t, s.PutIP(ctx, sampleIP("10.1.2.3", "nc-persist")))
		s.Close()
	}

	s2, err := store.OpenNCStore(path, nil)
	require.NoError(t, err)
	defer s2.Close()

	nc, err := s2.GetNC(ctx, "nc-persist")
	require.NoError(t, err)
	assert.Equal(t, "nc-persist", nc.ID)

	ip, err := s2.GetIP(ctx, "10.1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "nc-persist", ip.NCID)
}

// ---- Concurrency ----

func TestBoltNCStore_ConcurrentReads(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	// Pre-populate.
	for i := 1; i <= 5; i++ {
		addr := net.IPv4(10, 0, 0, byte(i)).String()
		require.NoError(t, s.PutIP(ctx, sampleIP(addr, "nc-r")))
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.ListIPs(ctx)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
}

func TestBoltNCStore_ConcurrentWriteRead(t *testing.T) {
	ctx := context.Background()
	s := openTestNCStore(t)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		idx := i
		go func() {
			defer wg.Done()
			addr := net.IPv4(10, 0, 0, byte(idx+1)).String()
			_ = s.PutIP(ctx, sampleIP(addr, "nc-cw"))
		}()
		go func() {
			defer wg.Done()
			_, _ = s.ListIPs(ctx)
		}()
	}
	wg.Wait()
}

// ---- BucketReadWriter low-level API ----

func TestBoltNCStore_BucketReadWriter(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/brw.db"

	s, err := store.OpenNCStore(path, nil)
	require.NoError(t, err)
	defer s.Close()

	// Write and read through the low-level API into an existing bucket.
	bucket := []byte("meta")
	key := []byte("custom-key")
	val := []byte("custom-val")

	require.NoError(t, s.Put(bucket, key, val))

	got, err := s.Get(bucket, key)
	require.NoError(t, err)
	assert.Equal(t, val, got)

	// Delete.
	require.NoError(t, s.Delete(bucket, key))
	_, err = s.Get(bucket, key)
	require.Error(t, err)

	// ForEach on an existing bucket should not panic.
	_ = s.PutMeta(ctx, store.StoreMeta{NodeID: "n1"})
	count := 0
	require.NoError(t, s.ForEach(bucket, func(k, v []byte) error {
		count++
		return nil
	}))
	assert.Positive(t, count)
}

func TestBoltEndpointStore_BucketReadWriter(t *testing.T) {
	s := openTestEndpointStore(t)

	bucket := []byte("endpoints")
	require.NoError(t, s.Put(bucket, []byte("raw-key"), []byte("raw-val")))

	v, err := s.Get(bucket, []byte("raw-key"))
	require.NoError(t, err)
	assert.Equal(t, []byte("raw-val"), v)
}

// ---- Schema mismatch ----

func TestBoltNCStore_SchemaMismatch(t *testing.T) {
	path := t.TempDir() + "/mismatch.db"

	// Open with normal schema which writes version=1 to meta bucket.
	s, err := store.OpenNCStore(path, nil)
	require.NoError(t, err)
	s.Close()

	// Corrupt the version by writing a wrong value using the bbolt API directly.
	db, err := bolt.Open(path, 0o600, nil)
	require.NoError(t, err)
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("meta"))
		if b == nil {
			return nil
		}
		return b.Put([]byte("version"), []byte{0x99, 0x00})
	})
	require.NoError(t, err)
	db.Close()

	// Re-opening with the store package should return ErrSchemaMismatch.
	_, err = store.OpenNCStore(path, nil)
	require.ErrorIs(t, err, store.ErrSchemaMismatch)
}

func TestBoltEndpointStore_SchemaVersion(t *testing.T) {
	path := t.TempDir() + "/endpoint-schema.db"

	s, err := store.OpenEndpointStore(path, nil)
	require.NoError(t, err)
	s.Close()

	db, err := bolt.Open(path, 0o600, nil)
	require.NoError(t, err)
	defer db.Close()

	err = db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		require.NotNil(t, meta)
		v := meta.Get([]byte("version"))
		require.Len(t, v, 2)
		return nil
	})
	require.NoError(t, err)
}

func TestBoltEndpointStore_SchemaMismatch(t *testing.T) {
	path := t.TempDir() + "/endpoint-mismatch.db"

	s, err := store.OpenEndpointStore(path, nil)
	require.NoError(t, err)
	s.Close()

	db, err := bolt.Open(path, 0o600, nil)
	require.NoError(t, err)
	err = db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if meta == nil {
			return nil
		}
		return meta.Put([]byte("version"), []byte{0x99, 0x00})
	})
	require.NoError(t, err)
	db.Close()

	_, err = store.OpenEndpointStore(path, nil)
	require.ErrorIs(t, err, store.ErrSchemaMismatch)
}
