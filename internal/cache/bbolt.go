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

	"github.com/tooltropolis/silo/internal/project"
)

// Bucket names within each project's bbolt file.
var (
	contentBucket = []byte("content") // path -> content bytes (the warm cache)
	queueBucket   = []byte("queue")   // monotonic seq -> serialized PendingWrite
	metaBucket    = []byte("meta")    // file-level facts, e.g. which tenant owns it
)

// generationKey records which incarnation of a project owns this cache file.
var generationKey = []byte("generation")

// BoltCache is the default LocalCache. It keeps one bbolt file per project under
// a base directory (opened lazily on first use) with a content bucket for the
// warm cache and an append-only queue bucket for writes buffered while the
// durable backend was unreachable.
type BoltCache struct {
	baseDir string

	mu  sync.Mutex
	dbs map[string]*projectDB // projectID -> open handle
}

// projectDB is an open handle plus the generation it was verified against, so
// the check happens once per open rather than on every read.
type projectDB struct {
	db         *bolt.DB
	generation string
}

var _ LocalCache = (*BoltCache)(nil)

// NewBoltCache returns a cache rooted at baseDir (created if absent). One bbolt
// file per project lives under it, matching configs/example.yaml's cache.path.
func NewBoltCache(baseDir string) (*BoltCache, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, fmt.Errorf("cache: create base dir: %w", err)
	}
	return &BoltCache{baseDir: baseDir, dbs: make(map[string]*projectDB)}, nil
}

// db opens (once) and returns the bbolt handle for a project, initializing its
// buckets. Handles are cached so concurrent callers share one *bolt.DB, which
// bbolt serializes internally.
func (c *BoltCache) db(projectID string) (*bolt.DB, error) {
	// Validate before the ID reaches filepath.Join below. This is defence in
	// depth — onboarding already rejects bad IDs — but it is the last point
	// before an ID becomes a real path, and "../escape" here would create a
	// bbolt file outside the cache directory entirely.
	if err := project.ValidateID(projectID); err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.openLocked(projectID)
}

// openLocked returns the project's handle, opening it if needed. The caller
// must hold c.mu — BindProject already does, and taking it twice would
// deadlock.
func (c *BoltCache) openLocked(projectID string) (*bolt.DB, error) {
	if pdb, ok := c.dbs[projectID]; ok {
		return pdb.db, nil
	}
	path := filepath.Join(c.baseDir, projectID+".bbolt")
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("cache: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{contentBucket, queueBucket, metaBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("cache: init buckets: %w", err)
	}
	c.dbs[projectID] = &projectDB{db: db}
	return db, nil
}

// BindProject verifies that this project's cache file belongs to the given
// generation, discarding its contents if it does not.
//
// The cache file is named after the projectID, so a project torn down and later
// re-onboarded under the same ID inherits the previous tenant's file. Without
// this check the read path's outage fallback would serve the old tenant's
// memory to the new project, and the sync worker would replay the old tenant's
// queued writes into the new project's bucket.
//
// The check runs on open, not per read: once bound, the handle carries its
// verified generation and Get/Put cost exactly what they cost before.
//
// A file with no stamp predates generations. Its content is discarded — it
// cannot be proven to belong to this tenant — but its QUEUE is preserved, since
// those are unsynced writes and dropping them would be data loss.
func (c *BoltCache) BindProject(ctx context.Context, projectID, generation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if generation == "" {
		return fmt.Errorf("cache: bind %q: %w", projectID, ErrNoGeneration)
	}
	if err := project.ValidateID(projectID); err != nil {
		return fmt.Errorf("cache: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// An already-bound handle for a different generation means the project was
	// re-onboarded while this process was running. Close it so the reopen below
	// re-runs the check rather than serving the old tenant from a live handle.
	if pdb, ok := c.dbs[projectID]; ok {
		if pdb.generation == generation {
			return nil
		}
		_ = pdb.db.Close()
		delete(c.dbs, projectID)
	}

	db, err := c.openLocked(projectID)
	if err != nil {
		return err
	}

	wiped := 0
	err = db.Update(func(tx *bolt.Tx) error {
		stamped := tx.Bucket(metaBucket).Get(generationKey)
		if string(stamped) == generation {
			return nil // ours
		}
		// Either a previous tenant's file or a pre-generation one. Neither can
		// be shown to belong to this project, so the content goes.
		wiped = tx.Bucket(contentBucket).Stats().KeyN
		if err := tx.DeleteBucket(contentBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(contentBucket); err != nil {
			return err
		}
		// A stamped-but-different generation means the queue holds the previous
		// tenant's writes, which must never replay into this project's bucket.
		// An unstamped file predates generations, so its queue is this
		// project's own unsynced data and is kept.
		if len(stamped) > 0 {
			if err := tx.DeleteBucket(queueBucket); err != nil {
				return err
			}
			if _, err := tx.CreateBucket(queueBucket); err != nil {
				return err
			}
		}
		return tx.Bucket(metaBucket).Put(generationKey, []byte(generation))
	})
	if err != nil {
		_ = db.Close()
		delete(c.dbs, projectID)
		return fmt.Errorf("cache: bind %q: %w", projectID, err)
	}

	if wiped > 0 {
		// Loud on purpose: reaching this means a teardown purge did not happen,
		// and someone should know a previous tenant's data was sitting here.
		fmt.Printf("cache: %s: discarded %d cached entr(ies) belonging to a previous generation\n",
			projectID, wiped)
	}

	c.dbs[projectID].generation = generation
	return nil
}

// Close releases every open bbolt handle. Safe to call once at shutdown.
func (c *BoltCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var firstErr error
	for id, pdb := range c.dbs {
		if err := pdb.db.Close(); err != nil && firstErr == nil {
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

// QueueDepth reports how many writes are buffered for a project without
// consuming them.
//
// This exists because DrainQueue is destructive: it empties the bucket in the
// same transaction that reads it, so the only way to count the backlog was to
// destroy it. An operator asking "is it safe to shut down?" would have caused
// the data loss they were checking for.
//
// Read-only (db.View) and O(1)-ish via bucket statistics rather than walking and
// unmarshalling every entry, since callers only want the count.
func (c *BoltCache) QueueDepth(ctx context.Context, projectID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	db, err := c.db(projectID)
	if err != nil {
		return 0, err
	}
	var depth int
	err = db.View(func(tx *bolt.Tx) error {
		depth = tx.Bucket(queueBucket).Stats().KeyN
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("cache: queue depth %q: %w", projectID, err)
	}
	return depth, nil
}

// OldestQueued returns the QueuedAt stamp of the oldest buffered write, or ""
// when the queue is empty.
//
// Depth alone is hard to act on: "12 pending" reads very differently from "12
// pending, oldest 3 hours ago". Deliberately not on the LocalCache interface —
// it is a reporting nicety, and the interface should stay minimal.
func (c *BoltCache) OldestQueued(ctx context.Context, projectID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	db, err := c.db(projectID)
	if err != nil {
		return "", err
	}
	var oldest string
	err = db.View(func(tx *bolt.Tx) error {
		// Keys are a monotonic big-endian sequence, so the first key is the
		// oldest entry — no scan needed.
		_, v := tx.Bucket(queueBucket).Cursor().First()
		if v == nil {
			return nil
		}
		var w PendingWrite
		if err := json.Unmarshal(v, &w); err != nil {
			return fmt.Errorf("unmarshal pending write: %w", err)
		}
		oldest = w.QueuedAt
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("cache: oldest queued %q: %w", projectID, err)
	}
	return oldest, nil
}

// itob encodes a uint64 sequence as an 8-byte big-endian key so bbolt's
// byte-order iteration matches numeric order.
func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}
