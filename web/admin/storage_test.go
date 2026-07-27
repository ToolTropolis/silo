package admin

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/registry"
)

type fakeMemoryLister struct {
	paths []string
	err   error
	// versions is newest-first, matching the DurableBackend contract, so index 0
	// is the head the redaction view must refuse.
	versions    []backend.ObjectVersion
	versionsErr error
}

func (f *fakeMemoryLister) ListPaths(context.Context, string, string) ([]string, error) {
	return f.paths, f.err
}

func (f *fakeMemoryLister) ListVersions(context.Context, string, string) ([]backend.ObjectVersion, error) {
	return f.versions, f.versionsErr
}

func storageFixture(t *testing.T, mem MemoryLister, d DaemonAdmin) *httptest.Server {
	t.Helper()
	return newFixture(t, Config{
		Registry: &fakeRegistry{records: []registry.ProjectRecord{
			{ProjectID: "proj-11", BucketName: "silo-proj-11", Status: registry.StatusActive},
		}},
		Settings: newFakeSettings(nil),
		Memory:   mem,
		Daemon:   d,
	})
}

// The bucket is the durable copy; the console shows its structure.
func TestStorage_ListsBucketObjects(t *testing.T) {
	ts := storageFixture(t, &fakeMemoryLister{paths: []string{
		"memory/conventions.md", "memory/agents/style-reviewer.md",
	}}, nil)

	body := getBody(t, ts, "/project?project=proj-11")
	for _, want := range []string{"memory/conventions.md", "memory/agents/style-reviewer.md", "2 object(s)"} {
		if !strings.Contains(body, want) {
			t.Errorf("storage view missing %q", want)
		}
	}
}

// The cache is a separate question: a local copy that can lag or hold a write
// the bucket has not seen.
func TestStorage_ListsCacheEntriesAndFlagsUnsynced(t *testing.T) {
	ts := storageFixture(t, &fakeMemoryLister{}, &fakeDaemon{entries: []CacheEntry{
		{Path: "memory/a.md", Bytes: 120},
		{Path: "memory/pending.md", Bytes: 80, Queued: true},
	}})

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "memory/pending.md") {
		t.Error("cached paths should be listed")
	}
	// An unsynced entry exists only on this host — the one case where losing the
	// cache loses data.
	if !strings.Contains(body, "unsynced") {
		t.Error("a queued write should be flagged as unsynced")
	}
}

// A failing bucket listing must not blank the page: the cache half is still
// accurate and useful.
func TestStorage_BucketErrorIsReported(t *testing.T) {
	ts := storageFixture(t, &fakeMemoryLister{err: errors.New("no such bucket")},
		&fakeDaemon{entries: []CacheEntry{{Path: "memory/a.md", Bytes: 10}}})

	body := getBody(t, ts, "/project?project=proj-11")
	if !strings.Contains(body, "no such bucket") {
		t.Error("the bucket error should be surfaced")
	}
	if !strings.Contains(body, "memory/a.md") {
		t.Error("the cache half should still render")
	}
}

func TestStorage_AbsentWhenNothingConfigured(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/project?project=proj-11")
	if strings.Contains(body, "The bucket is the durable copy") {
		t.Error("no storage sources configured, so the panel should not render")
	}
}
