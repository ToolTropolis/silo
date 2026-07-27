package admin

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/registry"
)

// fakeRedactor records what the console asked for.
type fakeRedactor struct {
	mu       sync.Mutex
	calls    []string // "path@version:reason"
	err      error
	existing []registry.Redaction
}

func (f *fakeRedactor) RedactVersion(_ context.Context, _, path, versionID, reason, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, path+"@"+versionID+":"+reason)
	return nil
}

func (f *fakeRedactor) ListRedactions(context.Context, string, string) ([]registry.Redaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.existing, nil
}

func (f *fakeRedactor) applied() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func redactFixture(t *testing.T, mem MemoryLister, red Redactor) *httptest.Server {
	t.Helper()
	return newFixture(t, Config{
		Registry: activeProject("proj-11"),
		Settings: newFakeSettings(nil),
		Memory:   mem,
		Redact:   red,
	})
}

func twoVersions() *fakeMemoryLister {
	return &fakeMemoryLister{
		paths: []string{"memory/notes.md"},
		versions: []backend.ObjectVersion{
			{VersionID: "v-head"}, // newest-first
			{VersionID: "v-old"},
		},
	}
}

// The head must not be redactable, and the view has to say so before the
// operator commits — not refuse on submit.
func TestRedactView_HeadIsNotOfferedForRedaction(t *testing.T) {
	ts := redactFixture(t, twoVersions(), &fakeRedactor{})

	body := getBody(t, ts, "/redact?project=proj-11&path=memory/notes.md")
	if !strings.Contains(body, "v-head") || !strings.Contains(body, "v-old") {
		t.Fatal("both versions should be listed")
	}
	if !strings.Contains(body, "current") {
		t.Error("the head should be marked as the current version")
	}
	if !strings.Contains(body, "write a replacement first") {
		t.Error("the head row should explain why it cannot be redacted")
	}
}

// The typed confirmation is the guard against a misclick among near-identical
// opaque version IDs.
func TestRedact_RequiresTypedVersionConfirmation(t *testing.T) {
	red := &fakeRedactor{}
	ts := redactFixture(t, twoVersions(), red)

	postForm(t, ts, "/redact/apply", url.Values{
		"project": {"proj-11"}, "path": {"memory/notes.md"},
		"version": {"v-old"}, "reason": {"leaked key"},
		"confirm": {"v-wrong"},
	})
	if got := red.applied(); len(got) != 0 {
		t.Errorf("a mistyped confirmation still redacted: %v", got)
	}

	// The right value goes through.
	postForm(t, ts, "/redact/apply", url.Values{
		"project": {"proj-11"}, "path": {"memory/notes.md"},
		"version": {"v-old"}, "reason": {"leaked key"},
		"confirm": {"v-old"},
	})
	got := red.applied()
	if len(got) != 1 || got[0] != "memory/notes.md@v-old:leaked key" {
		t.Errorf("applied = %v, want the confirmed redaction with its reason", got)
	}
}

// A reason is the only record of why content was destroyed, so an empty one is
// refused rather than stored as a bare timestamp.
func TestRedact_RequiresAReason(t *testing.T) {
	red := &fakeRedactor{}
	ts := redactFixture(t, twoVersions(), red)

	postForm(t, ts, "/redact/apply", url.Values{
		"project": {"proj-11"}, "path": {"memory/notes.md"},
		"version": {"v-old"}, "confirm": {"v-old"}, "reason": {"  "},
	})
	if got := red.applied(); len(got) != 0 {
		t.Errorf("redacted without a reason: %v", got)
	}
}

// A daemon error — including "destroyed but not recorded" — must reach the
// operator verbatim, because that message says the content is already gone.
func TestRedact_SurfacesTheDaemonError(t *testing.T) {
	red := &fakeRedactor{err: errors.New(
		"daemon: redaction succeeded but was not recorded: memory/notes.md@v-old was destroyed")}
	ts := redactFixture(t, twoVersions(), red)

	resp := postForm(t, ts, "/redact/apply", url.Values{
		"project": {"proj-11"}, "path": {"memory/notes.md"},
		"version": {"v-old"}, "confirm": {"v-old"}, "reason": {"leak"},
	})
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "was+destroyed") && !strings.Contains(loc, "was%20destroyed") {
		t.Errorf("the redirect should carry the daemon's message, got %q", loc)
	}
}

// Already-redacted versions are shown: the record outlives the content, which
// is the whole reason it is stored in the registry.
func TestRedactView_ShowsExistingRedactions(t *testing.T) {
	red := &fakeRedactor{existing: []registry.Redaction{{
		VersionID: "v-gone", Reason: "contained an AWS key",
		RedactedAt: "2026-07-27T00:00:00Z", RedactedBy: "operator",
	}}}
	ts := redactFixture(t, twoVersions(), red)

	body := getBody(t, ts, "/redact?project=proj-11&path=memory/notes.md")
	for _, want := range []string{"v-gone", "contained an AWS key", "operator"} {
		if !strings.Contains(body, want) {
			t.Errorf("the redaction record should show %q", want)
		}
	}
}

// Without a redactor the view is read-only rather than broken.
func TestRedactView_WithoutARedactorIsReadOnly(t *testing.T) {
	ts := redactFixture(t, twoVersions(), nil)

	body := getBody(t, ts, "/redact?project=proj-11&path=memory/notes.md")
	if !strings.Contains(body, "not configured") {
		t.Error("the view should say redaction is unavailable")
	}
	if strings.Contains(body, "Destroy this version permanently") {
		t.Error("the destroy control must not render without a redactor")
	}
}

// GET must not destroy anything.
func TestRedact_RejectsGET(t *testing.T) {
	red := &fakeRedactor{}
	ts := redactFixture(t, twoVersions(), red)

	resp, err := noRedirectClient().Get(ts.URL + "/redact/apply?project=proj-11&path=x&version=v-old")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("GET /redact/apply = %d, want 405", resp.StatusCode)
	}
	if got := red.applied(); len(got) != 0 {
		t.Errorf("a GET redacted something: %v", got)
	}
}
