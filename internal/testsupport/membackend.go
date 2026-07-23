// Package testsupport provides in-memory test doubles shared across packages.
// It lives in internal/ so it never ships as public API, but is importable by
// any Silo package's tests (unlike fakes defined in a _test.go file).
package testsupport

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tooltropolis/silo/internal/backend"
)

// MemBackend is an in-memory DurableBackend with per-project namespacing and
// ETag CAS, suitable for exercising the daemon and SDK without SeaweedFS.
//
// Keys are namespaced by projectID, so it models the bucket-per-project
// isolation boundary: one project can never read another's objects.
type MemBackend struct {
	mu    sync.Mutex
	objs  map[string][]byte
	etags map[string]int
	down  bool
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{objs: map[string][]byte{}, etags: map[string]int{}}
}

var _ backend.DurableBackend = (*MemBackend)(nil)

// SetDown toggles an outage: every operation fails while down.
func (m *MemBackend) SetDown(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
}

// ErrDown is returned by every operation while the backend is "down".
var ErrDown = errDown{}

type errDown struct{}

func (errDown) Error() string { return "testsupport: backend down" }

func (m *MemBackend) key(projectID, path string) string { return projectID + "\x00" + path }

func (m *MemBackend) etag(n int) string { return "etag-" + strconv.Itoa(n) }

func (m *MemBackend) Get(_ context.Context, projectID, path, _ string) ([]byte, backend.ObjectVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, backend.ObjectVersion{}, ErrDown
	}
	k := m.key(projectID, path)
	v, ok := m.objs[k]
	if !ok {
		return nil, backend.ObjectVersion{}, backend.ErrNotFound
	}
	return append([]byte(nil), v...), backend.ObjectVersion{ETag: m.etag(m.etags[k])}, nil
}

func (m *MemBackend) Put(_ context.Context, projectID, path string, content []byte, opts backend.PutOptions) (backend.ObjectVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return backend.ObjectVersion{}, ErrDown
	}
	k := m.key(projectID, path)
	if opts.IfMatchETag != "" && opts.IfMatchETag != m.etag(m.etags[k]) {
		return backend.ObjectVersion{}, backend.ErrPreconditionFailed
	}
	m.etags[k]++
	m.objs[k] = append([]byte(nil), content...)
	return backend.ObjectVersion{ETag: m.etag(m.etags[k])}, nil
}

func (m *MemBackend) ListPaths(_ context.Context, projectID, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, ErrDown
	}
	want := projectID + "\x00"
	var out []string
	for k := range m.objs {
		if !strings.HasPrefix(k, want) {
			continue
		}
		p := strings.TrimPrefix(k, want)
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *MemBackend) ListVersions(context.Context, string, string) ([]backend.ObjectVersion, error) {
	return nil, nil
}

func (m *MemBackend) Delete(_ context.Context, projectID, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrDown
	}
	delete(m.objs, m.key(projectID, path))
	return nil
}

func (m *MemBackend) CreateBucket(context.Context, string) error { return nil }
func (m *MemBackend) DeleteBucket(context.Context, string) error { return nil }
