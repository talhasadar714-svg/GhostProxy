// Package storage provides a high-efficiency local disk I/O interface for
// persisting and retrieving HTTP response snapshots. It uses deterministic
// MD5 hashing of request method + URL path to generate stable, collision-
// resistant filenames within a local mappings directory. All filesystem
// operations include explicit error handling and cross-platform path safety.
package storage

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Snapshot represents a fully captured HTTP response at a specific point
// in time. It contains all the metadata necessary to faithfully replay
// the original response without any live network interaction.
type Snapshot struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
	CapturedAt string              `json:"captured_at"`
}

// Store is the exported interface for snapshot persistence. Accepting this
// interface (rather than *DiskStore directly) allows callers and tests to
// substitute any implementation — in-memory, remote, mock, etc.
type Store interface {
	Save(snap *Snapshot) error
	Load(method string, path string) (*Snapshot, bool, error)
	Exists(method string, path string) bool
	Delete(method string, path string) error
	ListSnapshots() ([]*Snapshot, error)
}

// diskStore is the concrete disk-backed implementation of Store. It manages
// a single directory on the local filesystem and uses a RWMutex to allow
// concurrent reads while serialising writes for goroutine safety.
type diskStore struct {
	baseDir string
	mu      sync.RWMutex
}

// NewDiskStore creates and initialises a new disk-backed Store rooted at
// the given directory path. The directory and all intermediate parents are
// created if they do not exist. Returns an error if the directory cannot
// be created or is not writable. The returned Store interface hides the
// unexported concrete type to prevent external coupling.
func NewDiskStore(directory string) (Store, error) {
	// filepath.Clean normalises separators for cross-platform safety.
	cleanDir := filepath.Clean(directory)

	if err := os.MkdirAll(cleanDir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: failed to create mappings directory %q: %w", cleanDir, err)
	}

	// Verify write access by creating and removing a temporary probe file.
	probePath := filepath.Join(cleanDir, ".ghostproxy_probe")
	probeFile, err := os.Create(probePath)
	if err != nil {
		return nil, fmt.Errorf("storage: mappings directory %q is not writable: %w", cleanDir, err)
	}
	probeFile.Close()
	// Best-effort removal; ignore error since probe file is benign.
	_ = os.Remove(probePath)

	return &diskStore{
		baseDir: cleanDir,
	}, nil
}

// Save persists a Snapshot to disk as a pretty-printed JSON file. The
// filename is derived from the deterministic MD5 hash of the HTTP method
// and request path, ensuring idempotent overwrites for the same route.
func (ds *diskStore) Save(snap *Snapshot) error {
	if snap == nil {
		return fmt.Errorf("storage: cannot save nil snapshot")
	}

	ds.mu.Lock()
	defer ds.mu.Unlock()

	snap.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("storage: failed to marshal snapshot for %s %s: %w",
			snap.Method, snap.Path, err)
	}

	filename := hashRoute(snap.Method, snap.Path)
	filePath := filepath.Join(ds.baseDir, filename)

	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return fmt.Errorf("storage: failed to write snapshot to %q: %w", filePath, err)
	}

	return nil
}

// Load retrieves a previously stored Snapshot from disk using the
// deterministic MD5 hash of the given HTTP method and path. Returns
// the deserialized Snapshot and true if found, or nil and false if
// no snapshot exists for this route.
func (ds *diskStore) Load(method string, path string) (*Snapshot, bool, error) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	filename := hashRoute(method, path)
	filePath := filepath.Join(ds.baseDir, filename)

	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("storage: failed to read snapshot from %q: %w", filePath, err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, false, fmt.Errorf("storage: failed to unmarshal snapshot from %q: %w", filePath, err)
	}

	return &snap, true, nil
}

// Exists checks whether a snapshot file exists on disk for the given
// HTTP method and path without reading or deserializing the contents.
func (ds *diskStore) Exists(method string, path string) bool {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	filename := hashRoute(method, path)
	filePath := filepath.Join(ds.baseDir, filename)

	_, err := os.Stat(filePath)
	return err == nil
}

// Delete removes a stored snapshot from disk for the given HTTP method
// and path. Returns nil if the file did not exist.
func (ds *diskStore) Delete(method string, path string) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	filename := hashRoute(method, path)
	filePath := filepath.Join(ds.baseDir, filename)

	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: failed to delete snapshot %q: %w", filePath, err)
	}

	return nil
}

// ListSnapshots reads the filenames from the storage directory under the
// read lock, then releases the lock before performing per-file I/O.
// This avoids holding the lock for the entire directory scan while
// still providing a consistent view of the inventory at call time.
func (ds *diskStore) ListSnapshots() ([]*Snapshot, error) {
	// Collect filenames under the read lock only.
	ds.mu.RLock()
	entries, err := os.ReadDir(ds.baseDir)
	ds.mu.RUnlock()

	if err != nil {
		return nil, fmt.Errorf("storage: failed to list mappings directory: %w", err)
	}

	var filePaths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			filePaths = append(filePaths, filepath.Join(ds.baseDir, entry.Name()))
		}
	}

	// Read and parse each snapshot file without holding the lock.
	snapshots := make([]*Snapshot, 0, len(filePaths))
	for _, fp := range filePaths {
		data, err := os.ReadFile(fp)
		if err != nil {
			continue // skip unreadable files gracefully
		}

		var snap Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			continue // skip malformed files gracefully
		}

		snapshots = append(snapshots, &snap)
	}

	return snapshots, nil
}

// hashRoute produces a deterministic MD5 hex digest from the combination
// of HTTP method and URL path. The result is used as a stable filename for
// snapshot persistence. Defined as a package-level function (not a method)
// so it can be used without a diskStore receiver and avoids per-call
// allocation that a method value would introduce.
func hashRoute(method string, path string) string {
	normalizedMethod := strings.ToUpper(strings.TrimSpace(method))
	normalizedPath := strings.TrimSpace(path)
	composite := normalizedMethod + "::" + normalizedPath
	hash := md5.Sum([]byte(composite))
	return hex.EncodeToString(hash[:]) + ".json"
}
