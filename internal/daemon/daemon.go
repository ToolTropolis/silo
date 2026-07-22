// Package daemon is the core process: session handling, the CAS write path,
// leader election / per-project locking, and the local write queue used when
// the durable backend is unreachable. It wires cache + backend + registry + kms
// together behind the daemon process.
package daemon

import (
	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/kms"
	"github.com/tooltropolis/silo/internal/registry"
)

// Daemon holds the wired-together dependencies of the running process.
type Daemon struct {
	backend  backend.DurableBackend
	cache    cache.LocalCache
	registry registry.TenantRegistry
	kms      kms.KeyManager

	// Leadership coordination (optional): set via WithLock. When present, the
	// daemon can acquire a per-project lock so only one instance owns the write
	// path for a project at a time.
	locker     registry.Locker
	instanceID string
}

// New constructs a Daemon from its dependencies. Leader election is opt-in via
// WithLock; session handling is wired in as those pieces land (build sequence
// step 3, docs/architecture.md).
func New(b backend.DurableBackend, c cache.LocalCache, r registry.TenantRegistry, k kms.KeyManager) *Daemon {
	return &Daemon{backend: b, cache: c, registry: r, kms: k}
}

// WithLock configures per-project leadership coordination: locker is the
// linearizable lock store (the rqlite registry satisfies it) and instanceID
// uniquely identifies this daemon process. Returns the daemon for chaining.
func (d *Daemon) WithLock(locker registry.Locker, instanceID string) *Daemon {
	d.locker = locker
	d.instanceID = instanceID
	return d
}
