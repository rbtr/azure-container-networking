package store

import (
	"fmt"
	"strings"

	"github.com/Azure/azure-container-networking/processlock"
	"go.uber.org/zap"
)

// Backend identifies a KeyValueStore implementation.
const (
	BackendJSON   = "json"
	BackendBoltDB = "bbolt"
	BackendSQLite = "sqlite"
)

// FileExtensionForBackend returns the file extension appropriate for the backend.
func FileExtensionForBackend(backend string) string {
	switch strings.ToLower(backend) {
	case BackendBoltDB, "bolt", "boltdb":
		return ".db"
	case BackendSQLite, "sqlite3":
		return ".sqlite"
	default:
		return ".json"
	}
}

// NewStore creates a KeyValueStore for the given backend type.
//
// For the JSON backend, lockclient must be non-nil. For bbolt and sqlite backends,
// lockclient is unused (concurrency is handled internally by the database).
func NewStore(backend, basePath string, lockclient processlock.Interface, logger *zap.Logger) (KeyValueStore, error) {
	ext := FileExtensionForBackend(backend)
	filePath := basePath + ext

	switch strings.ToLower(backend) {
	case BackendBoltDB, "bolt", "boltdb":
		return NewBoltStore(filePath)
	case BackendSQLite, "sqlite3":
		return NewSQLiteStore(filePath)
	case BackendJSON, "":
		return NewJsonFileStore(filePath, lockclient, logger)
	default:
		return nil, fmt.Errorf("unknown store backend %q: valid options are %q, %q, %q", backend, BackendJSON, BackendBoltDB, BackendSQLite)
	}
}
