// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Sentinel errors
var (
	ErrNotFound       = errors.New("record not found")
	ErrSchemaMismatch = errors.New("boltdb schema version mismatch")
)

// boltNCStore implements NCStore and BucketReadWriter using a boltdb file.
type boltNCStore struct {
	db *bolt.DB
}

// boltEndpointStore implements EndpointStore and BucketReadWriter using a
// separate boltdb file.
type boltEndpointStore struct {
	db *bolt.DB
}

// compile-time interface checks
var (
	_ NCStore          = (*boltNCStore)(nil)
	_ BucketReadWriter = (*boltNCStore)(nil)
	_ EndpointStore    = (*boltEndpointStore)(nil)
	_ BucketReadWriter = (*boltEndpointStore)(nil)
)

// cnsDBBuckets are the bucket names initialised in the CNS state DB.
var cnsDBBuckets = []string{
	bucketMeta,
	bucketNetworkContainers,
	bucketIPs,
	bucketNetworks,
	bucketOrchestratorContext,
	bucketPnpMAC,
}

// endpointDBBuckets are the bucket names initialised in the endpoint DB.
var endpointDBBuckets = []string{
	bucketMeta,
	bucketEndpoints,
}

// OpenNCStore opens (or creates) a boltdb file at path and returns an NCStore.
// The caller must call Close() when done.
func OpenNCStore(path string, opts *bolt.Options) (NCStore, error) {
	db, err := bolt.Open(path, 0o600, opts)
	if err != nil {
		return nil, fmt.Errorf("cns/store: open NC store %q: %w", path, err)
	}

	s := &boltNCStore{db: db}
	if err := s.init(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return s, nil
}

// OpenEndpointStore opens (or creates) a boltdb file at path and returns an
// EndpointStore.  The caller must call Close() when done.
func OpenEndpointStore(path string, opts *bolt.Options) (EndpointStore, error) {
	db, err := bolt.Open(path, 0o600, opts)
	if err != nil {
		return nil, fmt.Errorf("cns/store: open endpoint store %q: %w", path, err)
	}

	s := &boltEndpointStore{db: db}
	if err := s.init(); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return s, nil
}

// NCStoreBucketReadWriter returns the BucketReadWriter view of an NCStore.
// Returns nil if s does not implement BucketReadWriter.
func NCStoreBucketReadWriter(s NCStore) BucketReadWriter {
	brw, _ := s.(BucketReadWriter)
	return brw
}

// EndpointStoreBucketReadWriter returns the BucketReadWriter view of an
// EndpointStore.  Returns nil if s does not implement BucketReadWriter.
func EndpointStoreBucketReadWriter(s EndpointStore) BucketReadWriter {
	brw, _ := s.(BucketReadWriter)
	return brw
}

// ---- boltNCStore initialisation ----

func (s *boltNCStore) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range cnsDBBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %q: %w", name, err)
			}
		}

		meta := tx.Bucket([]byte(bucketMeta))

		existing := meta.Get([]byte(metaKeyVersion))
		if existing == nil {
			// First open — write the schema version.
			return meta.Put([]byte(metaKeyVersion), uint16ToBytes(SchemaVersion))
		}

		v := bytesToUint16(existing)
		if v != SchemaVersion {
			return fmt.Errorf("%w: file has version %d, code expects %d",
				ErrSchemaMismatch, v, SchemaVersion)
		}
		return nil
	})
}

// ---- boltEndpointStore initialisation ----

func (s *boltEndpointStore) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range endpointDBBuckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return fmt.Errorf("create bucket %q: %w", name, err)
			}
		}

		meta := tx.Bucket([]byte(bucketMeta))

		existing := meta.Get([]byte(metaKeyVersion))
		if existing == nil {
			return meta.Put([]byte(metaKeyVersion), uint16ToBytes(SchemaVersion))
		}

		v := bytesToUint16(existing)
		if v != SchemaVersion {
			return fmt.Errorf("%w: file has version %d, code expects %d",
				ErrSchemaMismatch, v, SchemaVersion)
		}
		return nil
	})
}

// ===== NCStore implementation =====

func (s *boltNCStore) Close() error {
	return s.db.Close()
}

// --- StoreMeta ---

func (s *boltNCStore) PutMeta(_ context.Context, m StoreMeta) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketMeta))
		puts := []struct{ k, v string }{
			{metaKeyOrchestratorType, m.OrchestratorType},
			{metaKeyNodeID, m.NodeID},
			{metaKeyLocation, m.Location},
			{metaKeyNetworkType, m.NetworkType},
		}
		for _, p := range puts {
			if err := b.Put([]byte(p.k), []byte(p.v)); err != nil {
				return err
			}
		}

		init := []byte{0}
		if m.Initialized {
			init = []byte{1}
		}
		if err := b.Put([]byte(metaKeyInitialized), init); err != nil {
			return err
		}

		ts, err := m.TimeStamp.MarshalBinary()
		if err != nil {
			return err
		}
		return b.Put([]byte(metaKeyTimestamp), ts)
	})
}

func (s *boltNCStore) GetMeta(_ context.Context) (StoreMeta, error) {
	var m StoreMeta
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketMeta))
		if v := b.Get([]byte(metaKeyVersion)); v != nil {
			m.Version = bytesToUint16(v)
		}
		if v := b.Get([]byte(metaKeyOrchestratorType)); v != nil {
			m.OrchestratorType = string(v)
		}
		if v := b.Get([]byte(metaKeyNodeID)); v != nil {
			m.NodeID = string(v)
		}
		if v := b.Get([]byte(metaKeyLocation)); v != nil {
			m.Location = string(v)
		}
		if v := b.Get([]byte(metaKeyNetworkType)); v != nil {
			m.NetworkType = string(v)
		}
		if v := b.Get([]byte(metaKeyInitialized)); v != nil {
			m.Initialized = len(v) > 0 && v[0] == 1
		}
		if v := b.Get([]byte(metaKeyTimestamp)); v != nil {
			_ = m.TimeStamp.UnmarshalBinary(v) // best-effort; zero value on failure
		}
		return nil
	})
	return m, err
}

// --- Network Containers ---

func (s *boltNCStore) PutNC(_ context.Context, nc NCRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketNetworkContainers))
		v, err := json.Marshal(nc)
		if err != nil {
			return err
		}
		return b.Put([]byte(nc.ID), v)
	})
}

func (s *boltNCStore) GetNC(_ context.Context, ncID string) (NCRecord, error) {
	var nc NCRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketNetworkContainers)).Get([]byte(ncID))
		if v == nil {
			return fmt.Errorf("NC %q: %w", ncID, ErrNotFound)
		}
		return json.Unmarshal(v, &nc)
	})
	return nc, err
}

func (s *boltNCStore) DeleteNC(_ context.Context, ncID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketNetworkContainers)).Delete([]byte(ncID))
	})
}

func (s *boltNCStore) ListNCs(_ context.Context) ([]NCRecord, error) {
	var ncs []NCRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketNetworkContainers)).ForEach(func(_, v []byte) error {
			var nc NCRecord
			if err := json.Unmarshal(v, &nc); err != nil {
				return err
			}
			ncs = append(ncs, nc)
			return nil
		})
	})
	return ncs, err
}

// --- Secondary IPs ---

func (s *boltNCStore) PutIP(_ context.Context, ip IPRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v, err := json.Marshal(ip)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketIPs)).Put([]byte(ip.IPAddress), v)
	})
}

func (s *boltNCStore) GetIP(_ context.Context, ipAddr string) (IPRecord, error) {
	var ip IPRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketIPs)).Get([]byte(ipAddr))
		if v == nil {
			return fmt.Errorf("IP %q: %w", ipAddr, ErrNotFound)
		}
		return json.Unmarshal(v, &ip)
	})
	return ip, err
}

func (s *boltNCStore) DeleteIP(_ context.Context, ipAddr string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketIPs)).Delete([]byte(ipAddr))
	})
}

// DeleteIPsByNCID scans the IPs bucket and removes every IPRecord whose NCID
// matches ncID.  The scan is O(n) in the total number of IPs, which is
// acceptable at AKS node scale (~500 IPs max).
func (s *boltNCStore) DeleteIPsByNCID(_ context.Context, ncID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketIPs))

		// Collect keys first; modifying the bucket while iterating is not
		// safe in boltdb.
		var toDelete [][]byte
		if err := b.ForEach(func(k, v []byte) error {
			var ip IPRecord
			if err := json.Unmarshal(v, &ip); err != nil {
				return err
			}
			if ip.NCID == ncID {
				key := make([]byte, len(k))
				copy(key, k)
				toDelete = append(toDelete, key)
			}
			return nil
		}); err != nil {
			return err
		}

		for _, k := range toDelete {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *boltNCStore) ListIPs(_ context.Context) ([]IPRecord, error) {
	var ips []IPRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketIPs)).ForEach(func(_, v []byte) error {
			var ip IPRecord
			if err := json.Unmarshal(v, &ip); err != nil {
				return err
			}
			ips = append(ips, ip)
			return nil
		})
	})
	return ips, err
}

// --- Networks ---

func (s *boltNCStore) PutNetwork(_ context.Context, n NetworkRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v, err := json.Marshal(n)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketNetworks)).Put([]byte(n.NetworkName), v)
	})
}

func (s *boltNCStore) GetNetwork(_ context.Context, name string) (NetworkRecord, error) {
	var n NetworkRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketNetworks)).Get([]byte(name))
		if v == nil {
			return fmt.Errorf("network %q: %w", name, ErrNotFound)
		}
		return json.Unmarshal(v, &n)
	})
	return n, err
}

func (s *boltNCStore) DeleteNetwork(_ context.Context, name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketNetworks)).Delete([]byte(name))
	})
}

func (s *boltNCStore) ListNetworks(_ context.Context) ([]NetworkRecord, error) {
	var nets []NetworkRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketNetworks)).ForEach(func(_, v []byte) error {
			var n NetworkRecord
			if err := json.Unmarshal(v, &n); err != nil {
				return err
			}
			nets = append(nets, n)
			return nil
		})
	})
	return nets, err
}

// --- Orchestrator context ---

func (s *boltNCStore) PutOrchestratorContext(_ context.Context, key string, ncIDs []string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v, err := json.Marshal(ncIDs)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketOrchestratorContext)).Put([]byte(key), v)
	})
}

func (s *boltNCStore) GetOrchestratorContext(_ context.Context, key string) ([]string, error) {
	var ncIDs []string
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketOrchestratorContext)).Get([]byte(key))
		if v == nil {
			return fmt.Errorf("orchestrator context %q: %w", key, ErrNotFound)
		}
		if err := json.Unmarshal(v, &ncIDs); err != nil {
			return err
		}
		return nil
	})
	return ncIDs, err
}

func (s *boltNCStore) DeleteOrchestratorContext(_ context.Context, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketOrchestratorContext)).Delete([]byte(key))
	})
}

func (s *boltNCStore) ListOrchestratorContexts(_ context.Context) (map[string][]string, error) {
	result := make(map[string][]string)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketOrchestratorContext)).ForEach(func(k, v []byte) error {
			var ncIDs []string
			if err := json.Unmarshal(v, &ncIDs); err != nil {
				return err
			}
			result[string(k)] = ncIDs
			return nil
		})
	})
	return result, err
}

// --- PnP ID by MAC ---

func (s *boltNCStore) PutPnpIDByMAC(_ context.Context, mac, pnpID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketPnpMAC)).Put([]byte(mac), []byte(pnpID))
	})
}

func (s *boltNCStore) GetPnpIDByMAC(_ context.Context, mac string) (string, error) {
	var pnpID string
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketPnpMAC)).Get([]byte(mac))
		if v == nil {
			return fmt.Errorf("MAC %q: %w", mac, ErrNotFound)
		}
		pnpID = string(v)
		return nil
	})
	return pnpID, err
}

func (s *boltNCStore) ListPnpIDByMAC(_ context.Context) (map[string]string, error) {
	result := make(map[string]string)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketPnpMAC)).ForEach(func(k, v []byte) error {
			result[string(k)] = string(v)
			return nil
		})
	})
	return result, err
}

// ===== BucketReadWriter implementation (boltNCStore) =====

func (s *boltNCStore) Get(bucket, key []byte) ([]byte, error) {
	var val []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %q: %w", bucket, ErrNotFound)
		}
		v := b.Get(key)
		if v == nil {
			return fmt.Errorf("key %q in bucket %q: %w", key, bucket, ErrNotFound)
		}
		val = make([]byte, len(v))
		copy(val, v)
		return nil
	})
	return val, err
}

func (s *boltNCStore) Put(bucket, key, value []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		return b.Put(key, value)
	})
}

func (s *boltNCStore) Delete(bucket, key []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil // nothing to delete
		}
		return b.Delete(key)
	})
}

func (s *boltNCStore) ForEach(bucket []byte, fn func(k, v []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %q: %w", bucket, ErrNotFound)
		}
		return b.ForEach(fn)
	})
}

// ===== EndpointStore implementation =====

func (s *boltEndpointStore) Close() error {
	return s.db.Close()
}

func (s *boltEndpointStore) PutEndpoint(_ context.Context, containerID string, ep EndpointRecord) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v, err := json.Marshal(ep)
		if err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketEndpoints)).Put([]byte(containerID), v)
	})
}

func (s *boltEndpointStore) GetEndpoint(_ context.Context, containerID string) (EndpointRecord, error) {
	var ep EndpointRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketEndpoints)).Get([]byte(containerID))
		if v == nil {
			return fmt.Errorf("endpoint %q: %w", containerID, ErrNotFound)
		}
		return json.Unmarshal(v, &ep)
	})
	return ep, err
}

func (s *boltEndpointStore) DeleteEndpoint(_ context.Context, containerID string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketEndpoints)).Delete([]byte(containerID))
	})
}

func (s *boltEndpointStore) ListEndpoints(_ context.Context) (map[string]EndpointRecord, error) {
	result := make(map[string]EndpointRecord)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(bucketEndpoints)).ForEach(func(k, v []byte) error {
			var ep EndpointRecord
			if err := json.Unmarshal(v, &ep); err != nil {
				return err
			}
			result[string(k)] = ep
			return nil
		})
	})
	return result, err
}

// ===== BucketReadWriter implementation (boltEndpointStore) =====

func (s *boltEndpointStore) Get(bucket, key []byte) ([]byte, error) {
	var val []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %q: %w", bucket, ErrNotFound)
		}
		v := b.Get(key)
		if v == nil {
			return fmt.Errorf("key %q in bucket %q: %w", key, bucket, ErrNotFound)
		}
		val = make([]byte, len(v))
		copy(val, v)
		return nil
	})
	return val, err
}

func (s *boltEndpointStore) Put(bucket, key, value []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucket)
		if err != nil {
			return err
		}
		return b.Put(key, value)
	})
}

func (s *boltEndpointStore) Delete(bucket, key []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return nil
		}
		return b.Delete(key)
	})
}

func (s *boltEndpointStore) ForEach(bucket []byte, fn func(k, v []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %q: %w", bucket, ErrNotFound)
		}
		return b.ForEach(fn)
	})
}

// ===== encoding helpers =====

func uint16ToBytes(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

func bytesToUint16(b []byte) uint16 {
	if len(b) < 2 {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

// timestampToBytes converts a time.Time to a reproducible byte slice for
// storage.  Returns nil on error.
func timestampToBytes(t time.Time) ([]byte, error) {
	return t.MarshalBinary()
}
