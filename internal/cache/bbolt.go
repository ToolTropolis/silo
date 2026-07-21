package cache

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Bucket names within each project's bbolt file.
var (
	contentBucket = []byte("content") // path -> content bytes (the warm cache)
	queueBucket   = []byte("queue")   // monotonic seq -> serialized PendingWrite
)

// BoltCache is the default LocalCache. It keeps one bbolt file per project under
// a base directory (opened lazily on first use) with a content bucket for the
// warm cache and an append-only queue bucket for writes buffered while the
// durable backend was unreachable.
type BoltCache struct {
	baseDir string

	mu  sync.Mutex
	dbs map[string]*bolt.DB // projectID -> open handle
}

var _ LocalCache = (*BoltCache)(nil)

// NewBoltCache returns a cache rooted at baseDir (created if absent). One bbolt
// file per project lives under it, matching configs/example.yaml's cache.path.
func NewBoltCache(baseDir string) (*BoltCache, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, fmt.Errorf("cache: create base dir: %w", err)
	}
	return &BoltCache{baseDir: baseDir, dbs: make(map[string]*bolt.DB)}, nil
}

// db opens (once) and returns the bbolt handle for a project, initializing its
// buckets. Handles are cached so concurrent callers share one *bolt.DB, which
// bbolt serializes internally.
func (c *BoltCache) db(projectID string) (*bolt.DB, error) {
	if projectID == "" {
		return nil, fmt.Errorf("cache: empty projectID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if db, ok := c.dbs[projectID]; ok {
		return db, nil
	}
	path := filepath.Join(c.baseDir, projectID+".bbolt")
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("cache: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{contentBucket, queueBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: init buckets: %w", err)
	}
	c.dbs[projectID] = db
	return db, nil
}

// Close releases every open bbolt handle. Safe to call once at shutdown.
func (c *BoltCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	for id, db := range c.dbs {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(c.dbs, id)
	}
	return firstErr
}

// Get returns the cached content at path, or ErrNotFound.
func (c *BoltCache) Get(ctx context.Context, projectID, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db, err := c.db(projectID)
	if err != nil {
		return nil, err
	}
	var out []byte
	err = db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(contentBucket).Get([]byte(path))
		if v == nil {
			return ErrNotFound
		}
		// bbolt values are only valid within the txn — copy before returning.
		out = append([]byte(nil), v...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Put writes content at path (overwriting any existing value).
func (c *BoltCache) Put(ctx context.Context, projectID, path string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	db, err := c.db(projectID)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(contentBucket).Put([]byte(path), content)
	})
}

// Delete removes the cached content at path. Deleting a missing key is a no-op.
func (c *BoltCache) Delete(ctx context.Context, projectID, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	db, err := c.db(projectID)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(contentBucket).Delete([]byte(path))
	})
}

// Enqueue appends a pending write to the project's offline queue. QueuedAt is
// stamped here if the caller left it empty, so drain order and audit are
// consistent regardless of caller discipline.
func (c *BoltCache) Enqueue(ctx context.Context, projectID string, w PendingWrite) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.QueuedAt == "" {
		w.QueuedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	blob, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("cache: marshal pending write: %w", err)
	}
	db, err := c.db(projectID)
	if err != nil {
		return err
	}
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(queueBucket)
		seq, err := b.NextSequence() // monotonic — preserves FIFO order
		if err != nil {
			return err
		}
		return b.Put(itob(seq), blob)
	})
}

// DrainQueue returns all pending writes in FIFO order and clears the queue in
// the same transaction, so a concurrent Enqueue either lands fully before the
// drain (and is returned) or fully after (and survives). Returns an empty slice
// when the queue is empty.
func (c *BoltCache) DrainQueue(ctx context.Context, projectID string) ([]PendingWrite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	db, err := c.db(projectID)
	if err != nil {
		return nil, err
	}
	var writes []PendingWrite
	err = db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(queueBucket)
		if err := b.ForEach(func(_, v []byte) error {
			var w PendingWrite
			if err := json.Unmarshal(v, &w); err != nil {
				return fmt.Errorf("cache: unmarshal pending write: %w", err)
			}
			writes = append(writes, w)
			return nil
		}); err != nil {
			return err
		}
		// Clear the queue by recreating the bucket.
		if err := tx.DeleteBucket(queueBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucket(queueBucket)
		return err
	})
	if err != nil {
		return nil, err
	}
	return writes, nil
}

// itob encodes a uint64 sequence as an 8-byte big-endian key so bbolt's
// byte-order iteration matches numeric order.
func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
