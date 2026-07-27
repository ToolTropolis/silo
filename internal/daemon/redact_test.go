package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/cache"
	"github.com/tooltropolis/silo/internal/registry"
)

// versionedBackend keeps real version history so redaction can be exercised
// end to end. fakeBackend only tracks one current object.
type versionedBackend struct {
	mu sync.Mutex
	// versions is newest-first, matching the DurableBackend contract.
	versions  []backend.ObjectVersion
	bodies    map[string][]byte
	deleted   []string
	deleteErr error
	listErr   error
}

func newVersionedBackend(contents ...string) *versionedBackend {
	b := &versionedBackend{bodies: map[string][]byte{}}
	// Written oldest-first, so prepend to keep the slice newest-first.
	for i, c := range contents {
		id := string(rune('a' + i))
		b.versions = append([]backend.ObjectVersion{{VersionID: id, ETag: id}}, b.versions...)
		b.bodies[id] = []byte(c)
	}
	return b
}

func (b *versionedBackend) ListVersions(context.Context, string, string) ([]backend.ObjectVersion, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listErr != nil {
		return nil, b.listErr
	}
	return append([]backend.ObjectVersion(nil), b.versions...), nil
}

func (b *versionedBackend) DeleteVersion(_ context.Context, _, _, versionID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deleteErr != nil {
		return b.deleteErr
	}
	for i, v := range b.versions {
		if v.VersionID == versionID {
			b.versions = append(b.versions[:i], b.versions[i+1:]...)
			delete(b.bodies, versionID)
			b.deleted = append(b.deleted, versionID)
			return nil
		}
	}
	return backend.ErrNotFound
}

func (b *versionedBackend) deletedIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.deleted...)
}

// The rest of DurableBackend, unused by redaction.
func (b *versionedBackend) Get(context.Context, string, string, string) ([]byte, backend.ObjectVersion, error) {
	return nil, backend.ObjectVersion{}, backend.ErrNotFound
}
func (b *versionedBackend) Put(context.Context, string, string, []byte, backend.PutOptions) (backend.ObjectVersion, error) {
	return backend.ObjectVersion{}, nil
}
func (b *versionedBackend) ListPaths(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (b *versionedBackend) Delete(context.Context, string, string) error { return nil }
func (b *versionedBackend) CreateBucket(context.Context, string) error   { return nil }
func (b *versionedBackend) DeleteBucket(context.Context, string) error   { return nil }

// recordingAudit captures what was recorded.
type recordingAudit struct {
	mu      sync.Mutex
	records []registry.Redaction
	err     error
}

func (a *recordingAudit) RecordRedaction(_ context.Context, r registry.Redaction) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.records = append(a.records, r)
	return nil
}

func (a *recordingAudit) all() []registry.Redaction {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]registry.Redaction(nil), a.records...)
}

func newRedactDaemon(t *testing.T, be backend.DurableBackend, audit RedactionRecorder) *Daemon {
	t.Helper()
	c, err := cache.NewBoltCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewBoltCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	d := New(be, c, newGenRegistry(), nil)
	if audit != nil {
		d = d.WithRedactionAudit(audit)
	}
	return d
}

// The case the feature exists for: a credential written into memory, replaced
// by a clean version, and then removed from history.
func TestRedact_DestroysOneVersionAndRecordsIt(t *testing.T) {
	be := newVersionedBackend("SECRET_KEY=abc123", "clean content")
	audit := &recordingAudit{}
	d := newRedactDaemon(t, be, audit)

	// "a" is the older, leaked version; "b" is the head.
	if err := d.RedactVersion(context.Background(), "proj-a", "memory/notes.md",
		"a", "leaked API key", "operator"); err != nil {
		t.Fatalf("RedactVersion: %v", err)
	}

	if got := be.deletedIDs(); len(got) != 1 || got[0] != "a" {
		t.Errorf("deleted %v, want exactly [a]", got)
	}
	// The clean version survives.
	remaining, _ := be.ListVersions(context.Background(), "proj-a", "memory/notes.md")
	if len(remaining) != 1 || remaining[0].VersionID != "b" {
		t.Errorf("remaining versions = %v, want only the head", remaining)
	}

	records := audit.all()
	if len(records) != 1 {
		t.Fatalf("recorded %d redactions, want 1", len(records))
	}
	r := records[0]
	if r.VersionID != "a" || r.Path != "memory/notes.md" || r.ProjectID != "proj-a" {
		t.Errorf("audit row = %+v, want it to name the destroyed version", r)
	}
	if r.Reason != "leaked API key" || r.RedactedBy != "operator" {
		t.Errorf("audit row lost the reason or actor: %+v", r)
	}
}

// The head guard. Destroying the current version would silently revert the path
// to older content, and a reader would see a different memory with no
// indication anything happened.
func TestRedact_RefusesTheCurrentVersion(t *testing.T) {
	be := newVersionedBackend("old", "current")
	audit := &recordingAudit{}
	d := newRedactDaemon(t, be, audit)

	err := d.RedactVersion(context.Background(), "proj-a", "memory/notes.md",
		"b", "oops", "operator")
	if !errors.Is(err, ErrRedactHeadVersion) {
		t.Fatalf("redacting the head = %v, want ErrRedactHeadVersion", err)
	}
	if got := be.deletedIDs(); len(got) != 0 {
		t.Errorf("deleted %v — the head must survive a refused redaction", got)
	}
	if len(audit.all()) != 0 {
		t.Error("a refused redaction must not be recorded as having happened")
	}
	// The message has to say what to do instead.
	if !strings.Contains(err.Error(), "write a replacement") {
		t.Errorf("error %q should name the recovery", err)
	}
}

// A version that is not in the history — already redacted, or never existed.
func TestRedact_UnknownVersionIsReportedClearly(t *testing.T) {
	be := newVersionedBackend("old", "current")
	audit := &recordingAudit{}
	d := newRedactDaemon(t, be, audit)

	err := d.RedactVersion(context.Background(), "proj-a", "memory/notes.md",
		"nonexistent", "", "operator")
	if !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("unknown version = %v, want ErrVersionNotFound", err)
	}
	if len(be.deletedIDs()) != 0 {
		t.Error("nothing should have been deleted")
	}
}

// Without an audit sink, redaction is refused entirely: destroying content with
// no record is worse than not offering the feature.
func TestRedact_RefusedWithoutAnAuditSink(t *testing.T) {
	be := newVersionedBackend("old", "current")
	d := newRedactDaemon(t, be, nil) // no WithRedactionAudit

	if err := d.RedactVersion(context.Background(), "proj-a", "memory/notes.md",
		"a", "", "operator"); err == nil {
		t.Fatal("redaction without an audit sink must be refused")
	}
	if len(be.deletedIDs()) != 0 {
		t.Error("content was destroyed despite having nowhere to record it")
	}
}

// The bytes are destroyed before the audit row is written, so an audit failure
// leaves a redaction that happened but was not recorded. That must surface as
// its own error: reporting a plain failure would tell the operator nothing was
// removed about content that is already gone.
func TestRedact_AuditFailureIsLoudAndSaysContentIsGone(t *testing.T) {
	be := newVersionedBackend("SECRET", "clean")
	audit := &recordingAudit{err: errors.New("rqlite unreachable")}
	d := newRedactDaemon(t, be, audit)

	err := d.RedactVersion(context.Background(), "proj-a", "memory/notes.md",
		"a", "leak", "operator")
	if !errors.Is(err, ErrNoRedactionAudit) {
		t.Fatalf("audit failure = %v, want ErrNoRedactionAudit", err)
	}
	// The content really is gone — the error must not imply otherwise.
	if got := be.deletedIDs(); len(got) != 1 {
		t.Fatalf("deleted %v, want the version to have been destroyed", got)
	}
	if !strings.Contains(err.Error(), "destroyed") {
		t.Errorf("error %q must say the content is gone", err)
	}
	if !strings.Contains(err.Error(), "manually") {
		t.Errorf("error %q should tell the operator to record it by hand", err)
	}
}

// A backend that cannot list versions must not lead to a blind delete.
func TestRedact_ListFailureAbortsBeforeDeleting(t *testing.T) {
	be := newVersionedBackend("old", "current")
	be.listErr = errors.New("backend unreachable")
	audit := &recordingAudit{}
	d := newRedactDaemon(t, be, audit)

	if err := d.RedactVersion(context.Background(), "proj-a", "memory/notes.md",
		"a", "", "operator"); err == nil {
		t.Fatal("a list failure must abort the redaction")
	}
	if len(be.deletedIDs()) != 0 {
		t.Error("deleted a version without confirming it was not the head")
	}
}

// An empty version ID must never fall through to something that deletes the
// whole path.
func TestRedact_EmptyVersionIsRefused(t *testing.T) {
	be := newVersionedBackend("old", "current")
	d := newRedactDaemon(t, be, &recordingAudit{})

	if err := d.RedactVersion(context.Background(), "proj-a", "memory/notes.md",
		"", "", "operator"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("an empty version = %v, want ErrVersionNotFound", err)
	}
	if len(be.deletedIDs()) != 0 {
		t.Error("an empty version ID caused a delete")
	}
}

// Redacting the only version would leave the path with no content at all, and
// it is also the head — the guard covers it.
func TestRedact_SingleVersionIsTheHeadAndIsRefused(t *testing.T) {
	be := newVersionedBackend("only one")
	d := newRedactDaemon(t, be, &recordingAudit{})

	if err := d.RedactVersion(context.Background(), "proj-a", "memory/notes.md",
		"a", "", "operator"); !errors.Is(err, ErrRedactHeadVersion) {
		t.Errorf("redacting the only version = %v, want ErrRedactHeadVersion", err)
	}
	if len(be.deletedIDs()) != 0 {
		t.Error("the only version was destroyed")
	}
}
