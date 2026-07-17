// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

type ReadTx struct {
	tx *bolt.Tx
}

type WriteTx struct {
	ReadTx
}

func (r *ReadTx) Metadata() (Metadata, error) {
	metaBucket := r.tx.Bucket(bucketMetadata)
	meta := Metadata{
		SchemaVersion: bytesUint32(metaBucket.Get(metaKeySchemaVersion)),
		Authority:     Authority(metaBucket.Get(metaKeyAuthority)),
		Generation:    bytesUint64(metaBucket.Get(metaKeyGeneration)),
		BootID:        string(metaBucket.Get(metaKeyBootID)),
	}

	if data := metaBucket.Get(metaKeyService); data != nil {
		var serviceMeta Metadata
		if err := json.Unmarshal(data, &serviceMeta); err != nil {
			return Metadata{}, fmt.Errorf("decoding %q from bucket %q: %w", metaKeyService, bucketMetadata, err)
		}
		meta.OrchestratorType = serviceMeta.OrchestratorType
		meta.NodeID = serviceMeta.NodeID
		meta.Location = serviceMeta.Location
		meta.NetworkType = serviceMeta.NetworkType
		meta.Initialized = serviceMeta.Initialized
		meta.TimeStamp = serviceMeta.TimeStamp
	}

	return meta, nil
}

func (r *ReadTx) NetworkContainer(id string) (NetworkContainerRecord, error) {
	return getJSON[NetworkContainerRecord](r.tx, bucketNetworkContainers, id)
}

func (r *ReadTx) NetworkContainers() (map[string]NetworkContainerRecord, error) {
	return listJSON[NetworkContainerRecord](r.tx, bucketNetworkContainers)
}

func (r *ReadTx) IP(id string) (IPRecord, error) {
	return getJSON[IPRecord](r.tx, bucketIPs, id)
}

func (r *ReadTx) IPs() (map[string]IPRecord, error) {
	return listJSON[IPRecord](r.tx, bucketIPs)
}

func (r *ReadTx) Network(name string) (NetworkRecord, error) {
	return getJSON[NetworkRecord](r.tx, bucketNetworks, name)
}

func (r *ReadTx) Networks() (map[string]NetworkRecord, error) {
	return listJSON[NetworkRecord](r.tx, bucketNetworks)
}

func (r *ReadTx) OrchestratorContexts() (map[string][]string, error) {
	return listJSON[[]string](r.tx, bucketOrchestratorContexts)
}

func (r *ReadTx) PnPIDByMAC() (map[string]string, error) {
	result := make(map[string]string)
	err := forEach(r.tx, bucketPnPIDByMAC, func(key, value []byte) error {
		result[string(key)] = string(value)
		return nil
	})
	return result, err
}

func (r *ReadTx) Assignment(podKey string) (AssignmentRecord, error) {
	return getJSON[AssignmentRecord](r.tx, bucketAssignments, podKey)
}

func (r *ReadTx) Assignments() (map[string]AssignmentRecord, error) {
	return listJSON[AssignmentRecord](r.tx, bucketAssignments)
}

func (r *ReadTx) IPOwner(ipID string) (string, error) {
	value := r.tx.Bucket(bucketIPOwners).Get([]byte(ipID))
	if value == nil {
		return "", fmt.Errorf("IP owner %q: %w", ipID, ErrNotFound)
	}
	return string(value), nil
}

func (r *ReadTx) IPOwners() (map[string]string, error) {
	result := make(map[string]string)
	err := forEach(r.tx, bucketIPOwners, func(key, value []byte) error {
		result[string(key)] = string(value)
		return nil
	})
	return result, err
}

func (r *ReadTx) Endpoint(containerID string) (EndpointRecord, error) {
	return getJSON[EndpointRecord](r.tx, bucketEndpoints, containerID)
}

func (r *ReadTx) Endpoints() (map[string]EndpointRecord, error) {
	return listJSON[EndpointRecord](r.tx, bucketEndpoints)
}

func (r *ReadTx) DeleteIntent(containerID string) (DeleteIntent, error) {
	return getJSON[DeleteIntent](r.tx, bucketDeleteIntents, containerID)
}

func (r *ReadTx) DeleteIntents() (map[string]DeleteIntent, error) {
	return listJSON[DeleteIntent](r.tx, bucketDeleteIntents)
}

func (r *ReadTx) MigrationComplete() bool {
	return len(r.tx.Bucket(bucketMetadata).Get(metaKeyMigration)) != 0
}

func (r *ReadTx) RollbackComplete() bool {
	return len(r.tx.Bucket(bucketMetadata).Get(metaKeyRollback)) != 0
}

func (w *WriteTx) PutMetadata(meta Metadata) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encoding service metadata: %w", err)
	}
	bucket := w.tx.Bucket(bucketMetadata)
	if err := bucket.Put(metaKeyService, data); err != nil {
		return fmt.Errorf("writing service metadata: %w", err)
	}
	if meta.BootID != "" {
		if err := bucket.Put(metaKeyBootID, []byte(meta.BootID)); err != nil {
			return fmt.Errorf("writing boot ID: %w", err)
		}
	}
	if meta.Authority != "" {
		if err := bucket.Put(metaKeyAuthority, []byte(meta.Authority)); err != nil {
			return fmt.Errorf("writing state authority: %w", err)
		}
	}
	return nil
}

func (w *WriteTx) PutNetworkContainer(record NetworkContainerRecord) error {
	return putJSON(w.tx, bucketNetworkContainers, record.ID, record)
}

func (w *WriteTx) DeleteNetworkContainer(id string) error {
	return deleteKey(w.tx, bucketNetworkContainers, id)
}

func (w *WriteTx) PutIP(record IPRecord) error {
	return putJSON(w.tx, bucketIPs, record.ID, record)
}

func (w *WriteTx) DeleteIP(id string) error {
	return deleteKey(w.tx, bucketIPs, id)
}

func (w *WriteTx) PutNetwork(record NetworkRecord) error {
	return putJSON(w.tx, bucketNetworks, record.NetworkName, record)
}

func (w *WriteTx) DeleteNetwork(name string) error {
	return deleteKey(w.tx, bucketNetworks, name)
}

func (w *WriteTx) PutOrchestratorContext(key string, ncIDs []string) error {
	return putJSON(w.tx, bucketOrchestratorContexts, key, ncIDs)
}

func (w *WriteTx) DeleteOrchestratorContext(key string) error {
	return deleteKey(w.tx, bucketOrchestratorContexts, key)
}

func (w *WriteTx) PutPnPIDByMAC(mac, pnpID string) error {
	return putBytes(w.tx, bucketPnPIDByMAC, mac, []byte(pnpID))
}

func (w *WriteTx) DeletePnPIDByMAC(mac string) error {
	return deleteKey(w.tx, bucketPnPIDByMAC, mac)
}

func (w *WriteTx) PutAssignment(record AssignmentRecord) error {
	return putJSON(w.tx, bucketAssignments, record.Pod.PodKey, record)
}

func (w *WriteTx) DeleteAssignment(podKey string) error {
	return deleteKey(w.tx, bucketAssignments, podKey)
}

func (w *WriteTx) PutIPOwner(ipID, podKey string) error {
	return putBytes(w.tx, bucketIPOwners, ipID, []byte(podKey))
}

func (w *WriteTx) DeleteIPOwner(ipID string) error {
	return deleteKey(w.tx, bucketIPOwners, ipID)
}

func (w *WriteTx) PutEndpoint(containerID string, record EndpointRecord) error {
	return putJSON(w.tx, bucketEndpoints, containerID, record)
}

func (w *WriteTx) DeleteEndpoint(containerID string) error {
	return deleteKey(w.tx, bucketEndpoints, containerID)
}

func (w *WriteTx) PutDeleteIntent(containerID string, intent DeleteIntent) error {
	return putJSON(w.tx, bucketDeleteIntents, containerID, intent)
}

func (w *WriteTx) DeleteDeleteIntent(containerID string) error {
	return deleteKey(w.tx, bucketDeleteIntents, containerID)
}

func (w *WriteTx) SetMigrationComplete() error {
	return putBytes(w.tx, bucketMetadata, string(metaKeyMigration), []byte{1})
}

func (w *WriteTx) SetRollbackComplete() error {
	return putBytes(w.tx, bucketMetadata, string(metaKeyRollback), []byte{1})
}

func (w *WriteTx) ClearRollbackComplete() error {
	return deleteKey(w.tx, bucketMetadata, string(metaKeyRollback))
}

func (w *WriteTx) ClearAssignments() error {
	return w.clearBucket(bucketAssignments)
}

func (w *WriteTx) ClearIPOwners() error {
	return w.clearBucket(bucketIPOwners)
}

func (w *WriteTx) ClearEndpoints() error {
	return w.clearBucket(bucketEndpoints)
}

func (w *WriteTx) ClearDeleteIntents() error {
	return w.clearBucket(bucketDeleteIntents)
}

func (w *WriteTx) ClearState() error {
	for _, bucket := range [][]byte{
		bucketNetworkContainers,
		bucketIPs,
		bucketNetworks,
		bucketOrchestratorContexts,
		bucketPnPIDByMAC,
		bucketAssignments,
		bucketIPOwners,
		bucketEndpoints,
		bucketDeleteIntents,
	} {
		if err := w.clearBucket(bucket); err != nil {
			return err
		}
	}
	return nil
}

func (w *WriteTx) ClearDurableState() error {
	for _, bucket := range [][]byte{
		bucketNetworkContainers,
		bucketIPs,
		bucketNetworks,
		bucketOrchestratorContexts,
		bucketPnPIDByMAC,
	} {
		if err := w.clearBucket(bucket); err != nil {
			return err
		}
	}
	return nil
}

func (w *WriteTx) clearBucket(name []byte) error {
	if err := w.tx.DeleteBucket(name); err != nil {
		return fmt.Errorf("deleting bucket %q: %w", name, err)
	}
	if _, err := w.tx.CreateBucket(name); err != nil {
		return fmt.Errorf("creating bucket %q: %w", name, err)
	}
	return nil
}

func getJSON[T any](tx *bolt.Tx, bucket []byte, key string) (T, error) {
	var result T
	value := tx.Bucket(bucket).Get([]byte(key))
	if value == nil {
		return result, fmt.Errorf("%q in bucket %q: %w", key, bucket, ErrNotFound)
	}
	if err := json.Unmarshal(value, &result); err != nil {
		return result, fmt.Errorf("decoding %q from bucket %q: %w", key, bucket, err)
	}
	return result, nil
}

func listJSON[T any](tx *bolt.Tx, bucket []byte) (map[string]T, error) {
	result := make(map[string]T)
	err := forEach(tx, bucket, func(key, value []byte) error {
		var record T
		if err := json.Unmarshal(value, &record); err != nil {
			return fmt.Errorf("decoding %q from bucket %q: %w", key, bucket, err)
		}
		result[string(key)] = record
		return nil
	})
	return result, err
}

func forEach(tx *bolt.Tx, bucket []byte, fn func(key, value []byte) error) error {
	if err := tx.Bucket(bucket).ForEach(fn); err != nil {
		return fmt.Errorf("iterating bucket %q: %w", bucket, err)
	}
	return nil
}

func deleteKey(tx *bolt.Tx, bucket []byte, key string) error {
	if err := tx.Bucket(bucket).Delete([]byte(key)); err != nil {
		return fmt.Errorf("deleting %q from bucket %q: %w", key, bucket, err)
	}
	return nil
}

func putBytes(tx *bolt.Tx, bucket []byte, key string, value []byte) error {
	if err := tx.Bucket(bucket).Put([]byte(key), value); err != nil {
		return fmt.Errorf("writing %q to bucket %q: %w", key, bucket, err)
	}
	return nil
}

func putJSON(tx *bolt.Tx, bucket []byte, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encoding %q for bucket %q: %w", key, bucket, err)
	}
	if err := tx.Bucket(bucket).Put([]byte(key), data); err != nil {
		return fmt.Errorf("writing %q to bucket %q: %w", key, bucket, err)
	}
	return nil
}
