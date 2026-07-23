package distilator

import (
	"context"

	"github.com/tooltropolis/silo/internal/daemon"
)

// DaemonStore adapts the daemon to the Distilator's narrow Store interface.
// Reads and lists go through the daemon's read path; writes here are for run
// output only — promotion uses the daemon's SafeWrite via the Reviewer, not
// this Write method.
type DaemonStore struct {
	d *daemon.Daemon
}

// NewDaemonStore adapts a daemon for Distilator use.
func NewDaemonStore(d *daemon.Daemon) *DaemonStore { return &DaemonStore{d: d} }

var _ Store = (*DaemonStore)(nil)

func (s *DaemonStore) List(ctx context.Context, projectID, prefix string) ([]string, error) {
	return s.d.List(ctx, projectID, prefix)
}

func (s *DaemonStore) Read(ctx context.Context, projectID, path string) ([]byte, error) {
	return s.d.Read(ctx, projectID, path)
}

// Write persists run output. It routes through SafeWrite so even the output
// manifest gets versioning — the run namespace is append-only in practice, but
// there's no reason to bypass the CAS path.
func (s *DaemonStore) Write(ctx context.Context, projectID, path string, content []byte, actor, sessionID string) error {
	body := content
	return s.d.SafeWrite(ctx, projectID, path, func([]byte) []byte { return body }, actor, sessionID)
}
