package store

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

const boltBucketName = "kvstore"

// boltStore is a KeyValueStore backed by BoltDB (bbolt).
// BoltDB provides its own serialized write transactions, so no external
// mutex is needed for thread safety.
type boltStore struct {
	db       *bolt.DB
	filePath string
	mu       sync.RWMutex // only protects db pointer lifecycle (open/close), not data access
}

// NewBoltStore creates a new BoltDB-backed KeyValueStore.
func NewBoltStore(filePath string) (KeyValueStore, error) {
	if filePath == "" {
		return nil, fmt.Errorf("bolt store: file path is required")
	}

	db, err := bolt.Open(filePath, 0o600, &bolt.Options{
		Timeout:      1 * time.Second,
		NoGrowSync:   false,
		FreelistType: bolt.FreelistMapType,
	})
	if err != nil {
		return nil, fmt.Errorf("bolt store: failed to open db: %w", err)
	}

	// Create the default bucket.
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(boltBucketName))
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("bolt store: failed to create bucket: %w", err)
	}

	return &boltStore{
		db:       db,
		filePath: filePath,
	}, nil
}

func (bs *boltStore) Exists() bool {
	_, err := os.Stat(bs.filePath)
	return err == nil
}

func (bs *boltStore) Read(key string, value interface{}) error {
	return bs.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(boltBucketName))
		if b == nil {
			return ErrKeyNotFound
		}

		data := b.Get([]byte(key))
		if data == nil {
			return ErrKeyNotFound
		}

		return json.Unmarshal(data, value)
	})
}

func (bs *boltStore) Write(key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("bolt store: marshal error: %w", err)
	}

	return bs.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(boltBucketName))
		if b == nil {
			return fmt.Errorf("bolt store: bucket not found")
		}
		return b.Put([]byte(key), data)
	})
}

func (bs *boltStore) Flush() error {
	return bs.db.Sync()
}

// Lock is a no-op for BoltDB; transactions provide isolation.
func (bs *boltStore) Lock(_ time.Duration) error {
	return nil
}

// Unlock is a no-op for BoltDB.
func (bs *boltStore) Unlock() error {
	return nil
}

func (bs *boltStore) GetModificationTime() (time.Time, error) {
	info, err := os.Stat(bs.filePath)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime().UTC(), nil
}

func (bs *boltStore) Remove() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	bs.db.Close()
	os.Remove(bs.filePath)
}

// Close cleanly shuts down the BoltDB database.
func (bs *boltStore) Close() error {
	return bs.db.Close()
}
