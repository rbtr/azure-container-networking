package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewStore(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		wantErr bool
	}{
		{"json default", "json", false},
		{"json empty", "", false},
		{"bbolt", "bbolt", false},
		{"bolt alias", "bolt", false},
		{"sqlite", "sqlite", false},
		{"unknown", "redis", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			basePath := filepath.Join(dir, "test-store")
			s, err := NewStore(tt.backend, basePath, nil, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Verify basic write/read round-trip.
			if err := s.Write("key", "value"); err != nil {
				t.Fatalf("write failed: %v", err)
			}
			var result string
			if err := s.Read("key", &result); err != nil {
				t.Fatalf("read failed: %v", err)
			}
			if result != "value" {
				t.Fatalf("expected 'value', got %q", result)
			}
			// Cleanup.
			if c, ok := s.(interface{ Close() error }); ok {
				c.Close()
			}
			s.Remove()
		})
	}
}

func TestFileExtensionForBackend(t *testing.T) {
	tests := []struct {
		backend string
		want    string
	}{
		{"json", ".json"},
		{"", ".json"},
		{"bbolt", ".db"},
		{"bolt", ".db"},
		{"boltdb", ".db"},
		{"sqlite", ".sqlite"},
		{"sqlite3", ".sqlite"},
		{"BBOLT", ".db"},
		{"unknown", ".json"},
	}
	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			got := FileExtensionForBackend(tt.backend)
			if got != tt.want {
				t.Fatalf("FileExtensionForBackend(%q) = %q, want %q", tt.backend, got, tt.want)
			}
		})
	}
}

func TestNewStoreCreatesFile(t *testing.T) {
	backends := []string{"json", "bbolt", "sqlite"}
	for _, backend := range backends {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			basePath := filepath.Join(dir, "state")
			s, err := NewStore(backend, basePath, nil, nil)
			if err != nil {
				t.Fatal(err)
			}

			// Write something so the file exists on disk.
			if err := s.Write("init", true); err != nil {
				t.Fatal(err)
			}

			ext := FileExtensionForBackend(backend)
			expectedPath := basePath + ext
			if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
				t.Fatalf("expected file %s to exist", expectedPath)
			}

			if c, ok := s.(interface{ Close() error }); ok {
				c.Close()
			}
			s.Remove()
		})
	}
}
