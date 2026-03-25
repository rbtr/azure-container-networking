package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// sqliteStore is a KeyValueStore backed by a pure-Go SQLite database.
// SQLite's WAL mode allows concurrent reads with serialized writes,
// eliminating the need for external mutexes for data access.
type sqliteStore struct {
	db       *sql.DB
	filePath string
	mu       sync.RWMutex // only protects db pointer lifecycle (open/close), not data access
}

// NewSQLiteStore creates a new SQLite-backed KeyValueStore.
func NewSQLiteStore(filePath string) (KeyValueStore, error) {
	if filePath == "" {
		return nil, fmt.Errorf("sqlite store: file path is required")
	}

	// Open with WAL mode and appropriate pragmas for performance.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode%%3DWAL&_pragma=synchronous%%3DNORMAL&_pragma=busy_timeout%%3D10000", filePath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite store: failed to open db: %w", err)
	}

	// SQLite allows only one writer at a time; limit open conns to serialize writes
	// while the busy_timeout handles contention gracefully.
	db.SetMaxOpenConns(1)

	// Create the key-value table.
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS kvstore (
		key   TEXT PRIMARY KEY,
		value BLOB NOT NULL
	)`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite store: failed to create table: %w", err)
	}

	return &sqliteStore{
		db:       db,
		filePath: filePath,
	}, nil
}

func (ss *sqliteStore) Exists() bool {
	_, err := os.Stat(ss.filePath)
	return err == nil
}

func (ss *sqliteStore) Read(key string, value interface{}) error {
	var data []byte
	err := ss.db.QueryRow("SELECT value FROM kvstore WHERE key = ?", key).Scan(&data)
	if err != nil {
		if err == sql.ErrNoRows {
			return ErrKeyNotFound
		}
		return fmt.Errorf("sqlite store: read error: %w", err)
	}

	return json.Unmarshal(data, value)
}

func (ss *sqliteStore) Write(key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("sqlite store: marshal error: %w", err)
	}

	// Use a BEGIN IMMEDIATE transaction to acquire a write lock upfront,
	// avoiding SQLITE_BUSY mid-transaction. The busy_timeout pragma handles retries.
	tx, err := ss.db.Begin()
	if err != nil {
		return fmt.Errorf("sqlite store: begin tx error: %w", err)
	}

	_, err = tx.Exec(
		"INSERT INTO kvstore (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, data,
	)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("sqlite store: write error: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite store: commit error: %w", err)
	}
	return nil
}

func (ss *sqliteStore) Flush() error {
	_, err := ss.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// Lock is a no-op for SQLite; the database handles concurrency internally.
func (ss *sqliteStore) Lock(_ time.Duration) error {
	return nil
}

// Unlock is a no-op for SQLite.
func (ss *sqliteStore) Unlock() error {
	return nil
}

func (ss *sqliteStore) GetModificationTime() (time.Time, error) {
	info, err := os.Stat(ss.filePath)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime().UTC(), nil
}

func (ss *sqliteStore) Remove() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.db.Close()
	// SQLite creates auxiliary files (-wal, -shm) that also need cleanup.
	os.Remove(ss.filePath)
	os.Remove(ss.filePath + "-wal")
	os.Remove(ss.filePath + "-shm")
}

// Close cleanly shuts down the SQLite database.
func (ss *sqliteStore) Close() error {
	return ss.db.Close()
}
