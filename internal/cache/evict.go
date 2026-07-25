package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// EvictPolicy bounds a project's cached content.
//
// Zero means unlimited for each field, so a zero policy evicts nothing — the
// behaviour before eviction existed.
type EvictPolicy struct {
	// TTL discards entries older than this, measured from when they were
	// written.
	TTL time.Duration
	// MaxEntries caps the number of cached paths.
	MaxEntries int
	// MaxBytes caps the total size of cached content, headers included.
	MaxBytes int64
}

// Unlimited reports whether the policy would evict nothing.
func (p EvictPolicy) Unlimited() bool {
	return p.TTL <= 0 && p.MaxEntries <= 0 && p.MaxBytes <= 0
}

// EvictResult reports what one pass removed.
type EvictResult struct {
	Scanned      int
	EvictedTTL   int
	EvictedSize  int
	Pinned       int
	BytesAfter   int64
	EntriesAfter int
}

// Evicted is the total removed.
func (r EvictResult) Evicted() int { return r.EvictedTTL + r.EvictedSize }

// Evict applies a policy to one project's cached content.
//
// Only the content bucket is touched. The queue holds writes that never reached
// the backend, so evicting from it would be data loss rather than cache
// management — that separation is structural here, not a convention: this file
// names contentBucket and nothing else.
//
// Paths with a live queue entry are pinned. The write path caches content for a
// queued write so an agent can read back what it just wrote during an outage;
// dropping that while the queue entry survives would break exactly the offline
// case the cache exists for.
func (c *BoltCache) Evict(ctx context.Context, projectID string, policy EvictPolicy) (EvictResult, error) {
	var res EvictResult
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if policy.Unlimited() {
		return res, nil
	}
	db, err := c.db(projectID)
	if err != nil {
		return res, err
	}

	now := c.now()
	err = db.Update(func(tx *bolt.Tx) error {
		pinned, err := pinnedPaths(tx)
		if err != nil {
			return err
		}
		res.Pinned = len(pinned)

		type candidate struct {
			path      string
			writtenAt time.Time
			size      int
		}
		var (
			live      []candidate
			totalSize int64
			expired   [][]byte
		)

		content := tx.Bucket(contentBucket)
		if err := content.ForEach(func(k, v []byte) error {
			res.Scanned++
			path := string(k)
			_, writtenAt, decErr := decodeEntry(v)
			if decErr != nil {
				// Unreadable entries are useless — the backend can supply the
				// real content — so drop them rather than let them accumulate.
				expired = append(expired, append([]byte(nil), k...))
				return nil
			}
			if _, isPinned := pinned[path]; isPinned {
				totalSize += int64(len(v))
				return nil
			}
			if policy.TTL > 0 && now.Sub(writtenAt) > policy.TTL {
				expired = append(expired, append([]byte(nil), k...))
				return nil
			}
			live = append(live, candidate{path: path, writtenAt: writtenAt, size: len(v)})
			totalSize += int64(len(v))
			return nil
		}); err != nil {
			return err
		}

		for _, k := range expired {
			if err := content.Delete(k); err != nil {
				return err
			}
			res.EvictedTTL++
		}

		// Oldest first, so what goes under pressure is what has been least
		// recently refreshed. Every read from the backend rewrites the entry, so
		// a path in active use keeps a recent timestamp.
		sort.Slice(live, func(i, j int) bool { return live[i].writtenAt.Before(live[j].writtenAt) })

		// Count what remains after the TTL pass: the pinned entries, which are
		// never candidates, plus the live ones still standing.
		remaining := res.Pinned + len(live)

		for i := 0; i < len(live); i++ {
			overEntries := policy.MaxEntries > 0 && remaining > policy.MaxEntries
			overBytes := policy.MaxBytes > 0 && totalSize > policy.MaxBytes
			if !overEntries && !overBytes {
				break
			}
			if err := content.Delete([]byte(live[i].path)); err != nil {
				return err
			}
			totalSize -= int64(live[i].size)
			remaining--
			res.EvictedSize++
		}

		res.BytesAfter = totalSize
		// Counted rather than read back from Stats(): bucket statistics inside
		// the same write transaction do not reflect deletes that have not been
		// committed yet.
		res.EntriesAfter = remaining
		return nil
	})
	if err != nil {
		return EvictResult{}, fmt.Errorf("cache: evict %q: %w", projectID, err)
	}
	return res, nil
}

// pinnedPaths returns the paths that have writes waiting in the queue.
//
// Read from the same transaction as the eviction itself, so a write landing
// mid-pass cannot leave its content evicted and its queue entry orphaned.
func pinnedPaths(tx *bolt.Tx) (map[string]struct{}, error) {
	pinned := map[string]struct{}{}
	err := tx.Bucket(queueBucket).ForEach(func(_, v []byte) error {
		w, err := decodePendingWrite(v)
		if err != nil {
			// A queue entry we cannot parse still means something is pending;
			// it just cannot tell us which path, so nothing is pinned for it.
			return nil
		}
		pinned[w.Path] = struct{}{}
		return nil
	})
	return pinned, err
}

// CacheStats reports a project's cached content size, for deciding whether a
// policy is being approached and whether compaction is worthwhile.
type CacheStats struct {
	Entries   int
	Bytes     int64
	FileBytes int64 // on-disk size, which only shrinks on compaction
}

// Stats measures a project's cache without modifying it.
func (c *BoltCache) Stats(ctx context.Context, projectID string) (CacheStats, error) {
	var s CacheStats
	if err := ctx.Err(); err != nil {
		return s, err
	}
	db, err := c.db(projectID)
	if err != nil {
		return s, err
	}
	err = db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(contentBucket)
		s.Entries = b.Stats().KeyN
		return b.ForEach(func(_, v []byte) error {
			s.Bytes += int64(len(v))
			return nil
		})
	})
	if err != nil {
		return CacheStats{}, fmt.Errorf("cache: stats %q: %w", projectID, err)
	}
	if info, statErr := c.fileInfo(projectID); statErr == nil {
		s.FileBytes = info
	}
	return s, nil
}

// decodePendingWrite unmarshals a queue entry.
func decodePendingWrite(v []byte) (PendingWrite, error) {
	var w PendingWrite
	if err := json.Unmarshal(v, &w); err != nil {
		return PendingWrite{}, err
	}
	return w, nil
}

// fileInfo returns a project's cache file size on disk. Distinct from the sum
// of its entries: bbolt frees pages for reuse but never shrinks the file, so
// the two diverge as entries are evicted — which is what makes compaction
// worthwhile.
func (c *BoltCache) fileInfo(projectID string) (int64, error) {
	info, err := os.Stat(filepath.Join(c.baseDir, projectID+".bbolt"))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
