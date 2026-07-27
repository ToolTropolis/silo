package admin

// ProjectCacheStat is one project's local cache state, as reported by a daemon.
type ProjectCacheStat struct {
	Project string `json:"project"`
	Pending int    `json:"pending"`
	Oldest  string `json:"oldest_queued_at,omitempty"`
	Entries int    `json:"entries"`
	Bytes   int64  `json:"bytes"`
	// FileBytes is the on-disk size. The gap between it and Bytes is the disk
	// eviction freed but bbolt kept, which is exactly what compaction reclaims.
	FileBytes int64 `json:"file_bytes"`
	// StatsError explains why the size fields are absent, so the view can show
	// "unknown" rather than a fabricated zero.
	StatsError string `json:"stats_error,omitempty"`
}

// Reclaimable is the disk a compaction would return.
func (s ProjectCacheStat) Reclaimable() int64 {
	if s.FileBytes <= s.Bytes {
		return 0
	}
	return s.FileBytes - s.Bytes
}

// PurgeOutcome reports a purge attempt. A refusal is an outcome, not an error:
// declining to delete unsynced writes is the designed behaviour.
type PurgeOutcome struct {
	Purged  bool
	Pending int
	Reason  string
}

// CompactOutcome reports a compaction attempt, including a safe skip.
type CompactOutcome struct {
	Compacted   bool
	Reclaimed   int64
	BytesBefore int64
	BytesAfter  int64
	SkipReason  string
}

// TeardownStep is one layer of a project's teardown, with whether it still
// needs doing. Teardown stays per-layer here for the same reason it is in
// siloctl: each step is independently irreversible.
type TeardownStep struct {
	Name        string
	Description string
	Done        bool
	// Blocked explains why this step cannot run yet, e.g. unsynced writes still
	// buffered on a daemon's disk.
	Blocked string
}

// CacheEntry is one cached path on a daemon's disk. Content is deliberately
// absent: this answers "what is on this host", and streaming memory through an
// operator console is a different decision with different exposure.
type CacheEntry struct {
	Path      string `json:"path"`
	Bytes     int    `json:"bytes"`
	WrittenAt string `json:"written_at"`
	// Queued marks a path whose write has not reached the backend — the one
	// case where the cache holds something the bucket does not.
	Queued bool `json:"queued"`
}
