// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrNotFound          = errors.New("cns state: record not found")
	ErrSchemaMismatch    = errors.New("cns state: schema version mismatch")
	ErrDeleteIntent      = errors.New("cns state: endpoint delete intent exists")
	ErrIPAlreadyAssigned = errors.New("cns state: IP already assigned")
	ErrInconsistentState = errors.New("cns state: inconsistent state")
	ErrStaleGeneration   = errors.New("cns state: stale generation")
	ErrInvalidInput      = errors.New("cns state: invalid input")
)

const defaultOpenTimeout = 5 * time.Second

var (
	bucketMetadata             = []byte("metadata")
	bucketNetworkContainers    = []byte("network_containers")
	bucketIPs                  = []byte("ips")
	bucketNetworks             = []byte("networks")
	bucketOrchestratorContexts = []byte("orchestrator_contexts")
	bucketPnPIDByMAC           = []byte("pnp_id_by_mac")
	bucketAssignments          = []byte("assignments")
	bucketIPOwners             = []byte("ip_owners")
	bucketEndpoints            = []byte("endpoints")
	bucketDeleteIntents        = []byte("delete_intents")
)

var allBuckets = [][]byte{
	bucketMetadata,
	bucketNetworkContainers,
	bucketIPs,
	bucketNetworks,
	bucketOrchestratorContexts,
	bucketPnPIDByMAC,
	bucketAssignments,
	bucketIPOwners,
	bucketEndpoints,
	bucketDeleteIntents,
}

var (
	metaKeySchemaVersion = []byte("schema_version")
	metaKeyAuthority     = []byte("authority")
	metaKeyGeneration    = []byte("generation")
	metaKeyBootID        = []byte("boot_id")
	metaKeyService       = []byte("service")
	metaKeyMigration     = []byte("legacy_migration_complete")
	metaKeyRollback      = []byte("legacy_rollback_complete")
)

type Options struct {
	Timeout  time.Duration
	ReadOnly bool
	NoSync   bool
}

type DB struct {
	db *bolt.DB
}

func Open(path string, opts Options) (*DB, error) {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultOpenTimeout
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{
		Timeout:  timeout,
		ReadOnly: opts.ReadOnly,
		NoSync:   opts.NoSync,
	})
	if err != nil {
		return nil, fmt.Errorf("opening CNS state database %q: %w", path, err)
	}

	store := &DB{db: db}
	if !opts.ReadOnly {
		if err := store.initialize(); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return store, nil
}

func (s *DB) initialize() error {
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for _, name := range allBuckets {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return fmt.Errorf("creating bucket %q: %w", name, err)
			}
		}

		meta := tx.Bucket(bucketMetadata)
		version := meta.Get(metaKeySchemaVersion)
		if version == nil {
			if err := meta.Put(metaKeySchemaVersion, uint32Bytes(SchemaVersion)); err != nil {
				return fmt.Errorf("writing schema version: %w", err)
			}
			if err := meta.Put(metaKeyAuthority, []byte(AuthorityBolt)); err != nil {
				return fmt.Errorf("writing state authority: %w", err)
			}
			return meta.Put(metaKeyGeneration, uint64Bytes(0))
		}

		got := bytesUint32(version)
		if got != SchemaVersion {
			return fmt.Errorf("%w: database=%d code=%d", ErrSchemaMismatch, got, SchemaVersion)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("initializing CNS state database: %w", err)
	}
	return nil
}

func (s *DB) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("closing CNS state database: %w", err)
	}
	return nil
}

func (s *DB) View(ctx context.Context, fn func(*ReadTx) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("viewing CNS state: %w", err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		return fn(&ReadTx{tx: tx})
	}); err != nil {
		return fmt.Errorf("viewing CNS state: %w", err)
	}
	return nil
}

func (s *DB) Update(ctx context.Context, fn func(*WriteTx) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("updating CNS state: %w", err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		writeTx := &WriteTx{ReadTx: ReadTx{tx: tx}}
		if err := fn(writeTx); err != nil {
			return err
		}

		meta := tx.Bucket(bucketMetadata)
		generation := bytesUint64(meta.Get(metaKeyGeneration)) + 1
		if err := meta.Put(metaKeyGeneration, uint64Bytes(generation)); err != nil {
			return fmt.Errorf("updating state generation: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("updating CNS state: %w", err)
	}
	return nil
}

func uint32Bytes(value uint32) []byte {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint32(data, value)
	return data
}

func bytesUint32(data []byte) uint32 {
	if len(data) != 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(data)
}

func uint64Bytes(value uint64) []byte {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, value)
	return data
}

func bytesUint64(data []byte) uint64 {
	if len(data) != 8 {
		return 0
	}
	return binary.LittleEndian.Uint64(data)
}
