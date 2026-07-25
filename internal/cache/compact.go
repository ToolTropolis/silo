package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	bolt "go.etcd.io/bbolt"

	"github.com/tooltropolis/silo/internal/project"
)

// compactSuffix names the in-progress copy. Kept in the same directory as the
// live file so the final rename is atomic — across filesystems it would not be.
const compactSuffix = ".compact"

// CompactResult reports what a compaction reclaimed.
type CompactResult struct {
	BytesBefore int64
	BytesAfter  int64
	Skipped     bool
	SkipReason  string
}

// Reclaimed is the disk returned, which can be zero or negative on a file that
// was already dense.
func (r CompactResult) Reclaimed() int64 { return r.BytesBefore - r.BytesAfter }

// Compact rewrites a project's cache file, returning the space that eviction
// freed but bbolt kept.
//
// Deleting keys puts their pages on bbolt's freelist for reuse; the file itself
// never shrinks. So a project that cached a lot and then evicted most of it
// holds disk it is not using, and only a copy into a fresh file gives it back.
//
// Refuses while writes are queued. bbolt's Compact reads through a read
// transaction, so anything written during the copy is absent from the
// destination — tolerable for cache content, which the backend can supply
// again, but not for unsynced writes, which exist nowhere else. Skipping is
// benign: the disk is reclaimed on a later pass once the queue drains.
func (c *BoltCache) Compact(ctx context.Context, projectID string) (CompactResult, error) {
	var res CompactResult
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if err := project.ValidateID(projectID); err != nil {
		return res, fmt.Errorf("cache: %w", err)
	}

	live := filepath.Join(c.baseDir, projectID+".bbolt")
	tmp := live + compactSuffix

	// Check for the file before anything that would open it: QueueDepth creates
	// a cache file as a side effect, so asking it first would conjure one for a
	// project this host has never served.
	info, err := os.Stat(live)
	if err != nil {
		if os.IsNotExist(err) {
			res.Skipped = true
			res.SkipReason = "no cache file"
			return res, nil
		}
		return res, fmt.Errorf("cache: compact %q: %w", projectID, err)
	}
	res.BytesBefore = info.Size()

	depth, err := c.QueueDepth(ctx, projectID)
	if err != nil {
		return res, fmt.Errorf("cache: compact %q: %w", projectID, err)
	}
	if depth > 0 {
		res.Skipped = true
		res.SkipReason = fmt.Sprintf("%d write(s) queued", depth)
		return res, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	src, err := c.openLocked(projectID)
	if err != nil {
		return res, err
	}

	// A stale temp file from an interrupted run would make Open fail.
	_ = os.Remove(tmp)

	dst, err := bolt.Open(tmp, 0o600, &bolt.Options{Timeout: c.openTimeout})
	if err != nil {
		return res, fmt.Errorf("cache: compact %q: open destination: %w", projectID, err)
	}

	// From here on every failure path must remove the temp file and leave the
	// live handle exactly as it was. A cache holding a closed handle would fail
	// every subsequent request for this project.
	fail := func(err error) (CompactResult, error) {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return res, fmt.Errorf("cache: compact %q: %w", projectID, err)
	}

	if err := bolt.Compact(dst, src, 0); err != nil {
		return fail(err)
	}
	if err := dst.Close(); err != nil {
		return fail(err)
	}

	// Swap. The handle is closed first so the rename replaces a file nobody
	// holds open; on failure the entry is dropped from the map so the next
	// caller reopens from disk rather than reusing a closed handle.
	if pdb, ok := c.dbs[projectID]; ok {
		generation := pdb.generation
		if err := pdb.db.Close(); err != nil {
			delete(c.dbs, projectID)
			_ = os.Remove(tmp)
			return res, fmt.Errorf("cache: compact %q: close live handle: %w", projectID, err)
		}
		delete(c.dbs, projectID)

		if err := os.Rename(tmp, live); err != nil {
			_ = os.Remove(tmp)
			return res, fmt.Errorf("cache: compact %q: rename: %w", projectID, err)
		}

		reopened, err := c.openLocked(projectID)
		if err != nil {
			return res, fmt.Errorf("cache: compact %q: reopen: %w", projectID, err)
		}
		_ = reopened
		// The compacted copy carries the generation stamp with it, but restore
		// the in-memory binding so callers are not forced to re-bind.
		c.dbs[projectID].generation = generation
	}

	if info, err := os.Stat(live); err == nil {
		res.BytesAfter = info.Size()
	}
	return res, nil
}

// sweepCompactLeftovers removes temp files from an interrupted compaction.
//
// A crash before the rename leaves a complete but unreferenced copy; a crash
// after it leaves nothing, since the live file is already the compacted one.
// Either way the leftover is safe to delete, and leaving it would double the
// project's disk usage indefinitely.
func sweepCompactLeftovers(baseDir string) error {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), compactSuffix) {
			continue
		}
		if err := os.Remove(filepath.Join(baseDir, e.Name())); err != nil {
			return fmt.Errorf("cache: remove stale %s: %w", e.Name(), err)
		}
	}
	return nil
}
