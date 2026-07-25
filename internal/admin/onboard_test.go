package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/project"
	"github.com/tooltropolis/silo/internal/registry"
)

// --- fakes that record calls and can be told to fail on demand ---

type fakeRegistry struct {
	registered   bool
	deregistered bool
	refsUpdated  bool
	failRegister bool
	failRefs     bool
}

func (f *fakeRegistry) Register(context.Context, registry.ProjectRecord) error {
	if f.failRegister {
		return errors.New("register boom")
	}
	f.registered = true
	return nil
}
func (f *fakeRegistry) Get(context.Context, string) (registry.ProjectRecord, error) {
	return registry.ProjectRecord{}, nil
}
func (f *fakeRegistry) List(context.Context) ([]registry.ProjectRecord, error) { return nil, nil }
func (f *fakeRegistry) UpdateStatus(context.Context, string, string) error     { return nil }
func (f *fakeRegistry) UpdateRefs(context.Context, string, string, string) error {
	if f.failRefs {
		return errors.New("refs boom")
	}
	f.refsUpdated = true
	return nil
}
func (f *fakeRegistry) Deregister(context.Context, string) error {
	f.deregistered = true
	return nil
}
func (f *fakeRegistry) ClearBucket(context.Context, string) error { return nil }

type fakeKMS struct {
	created    bool
	revoked    bool
	failCreate bool
}

func (f *fakeKMS) CreateKey(context.Context, string) (string, error) {
	if f.failCreate {
		return "", errors.New("createkey boom")
	}
	f.created = true
	return "key-abc", nil
}
func (f *fakeKMS) GetKey(context.Context, string) ([]byte, error) { return []byte("k"), nil }
func (f *fakeKMS) RevokeKey(context.Context, string) error {
	f.revoked = true
	return nil
}

type fakeBackend struct {
	created    bool
	deleted    bool
	failCreate bool
	backend.DurableBackend
}

func (f *fakeBackend) CreateBucket(context.Context, string) error {
	if f.failCreate {
		return errors.New("createbucket boom")
	}
	f.created = true
	return nil
}
func (f *fakeBackend) DeleteBucket(context.Context, string) error {
	f.deleted = true
	return nil
}

type fakeCreds struct {
	issued    bool
	revoked   bool
	failIssue bool
}

func (f *fakeCreds) Issue(context.Context, string, string) (string, error) {
	if f.failIssue {
		return "", errors.New("issue boom")
	}
	f.issued = true
	return "cred-xyz", nil
}
func (f *fakeCreds) Revoke(context.Context, string) error {
	f.revoked = true
	return nil
}

func newOnboarder(r *fakeRegistry, k *fakeKMS, b *fakeBackend, c *fakeCreds) *Onboarder {
	return &Onboarder{Registry: r, KMS: k, Backend: b, Creds: c}
}

func TestOnboard_HappyPath(t *testing.T) {
	r, k, b, c := &fakeRegistry{}, &fakeKMS{}, &fakeBackend{}, &fakeCreds{}
	err := newOnboarder(r, k, b, c).Onboard(context.Background(), "proj-11")
	if err != nil {
		t.Fatalf("Onboard: %v", err)
	}
	if !(r.registered && k.created && b.created && c.issued && r.refsUpdated) {
		t.Fatalf("not all steps ran: reg=%v key=%v bucket=%v cred=%v refs=%v",
			r.registered, k.created, b.created, c.issued, r.refsUpdated)
	}
	if r.deregistered || k.revoked || b.deleted || c.revoked {
		t.Fatal("compensations ran on a successful onboard")
	}
}

// Each sub-test forces a failure at one step and asserts that exactly the
// already-completed steps are compensated (in reverse), and no later ones.
func TestOnboard_RollsBackOnFailure(t *testing.T) {
	t.Run("fail at register", func(t *testing.T) {
		r, k, b, c := &fakeRegistry{failRegister: true}, &fakeKMS{}, &fakeBackend{}, &fakeCreds{}
		if err := newOnboarder(r, k, b, c).Onboard(context.Background(), "proj-11"); err == nil {
			t.Fatal("want error")
		}
		// Nothing succeeded, so nothing to compensate.
		if r.deregistered || k.revoked || b.deleted || c.revoked {
			t.Fatal("compensated something that never succeeded")
		}
	})

	t.Run("fail at create key", func(t *testing.T) {
		r, k, b, c := &fakeRegistry{}, &fakeKMS{failCreate: true}, &fakeBackend{}, &fakeCreds{}
		if err := newOnboarder(r, k, b, c).Onboard(context.Background(), "proj-11"); err == nil {
			t.Fatal("want error")
		}
		if !r.deregistered {
			t.Fatal("registry not rolled back after key failure")
		}
		if k.revoked || b.deleted || c.revoked {
			t.Fatal("rolled back a step that never ran")
		}
	})

	t.Run("fail at create bucket", func(t *testing.T) {
		r, k, b, c := &fakeRegistry{}, &fakeKMS{}, &fakeBackend{failCreate: true}, &fakeCreds{}
		if err := newOnboarder(r, k, b, c).Onboard(context.Background(), "proj-11"); err == nil {
			t.Fatal("want error")
		}
		if !(r.deregistered && k.revoked) {
			t.Fatalf("expected registry+key rollback, got reg=%v key=%v", r.deregistered, k.revoked)
		}
		if b.deleted || c.revoked {
			t.Fatal("rolled back a step that never ran")
		}
	})

	t.Run("fail at issue credential", func(t *testing.T) {
		r, k, b, c := &fakeRegistry{}, &fakeKMS{}, &fakeBackend{}, &fakeCreds{failIssue: true}
		if err := newOnboarder(r, k, b, c).Onboard(context.Background(), "proj-11"); err == nil {
			t.Fatal("want error")
		}
		if !(r.deregistered && k.revoked && b.deleted) {
			t.Fatalf("expected reg+key+bucket rollback, got reg=%v key=%v bucket=%v",
				r.deregistered, k.revoked, b.deleted)
		}
		if c.revoked {
			t.Fatal("revoked a credential that was never issued")
		}
	})

	t.Run("fail at persist refs rolls back everything", func(t *testing.T) {
		r, k, b, c := &fakeRegistry{failRefs: true}, &fakeKMS{}, &fakeBackend{}, &fakeCreds{}
		if err := newOnboarder(r, k, b, c).Onboard(context.Background(), "proj-11"); err == nil {
			t.Fatal("want error")
		}
		if !(r.deregistered && k.revoked && b.deleted && c.revoked) {
			t.Fatalf("expected full rollback, got reg=%v key=%v bucket=%v cred=%v",
				r.deregistered, k.revoked, b.deleted, c.revoked)
		}
	})
}

func TestOnboard_EmptyProjectID(t *testing.T) {
	r, k, b, c := &fakeRegistry{}, &fakeKMS{}, &fakeBackend{}, &fakeCreds{}
	if err := newOnboarder(r, k, b, c).Onboard(context.Background(), ""); err == nil {
		t.Fatal("want error for empty projectID")
	}
	if r.registered {
		t.Fatal("registered despite empty projectID")
	}
}

// TestOnboard_RejectsInvalidProjectID: onboarding is the authoritative gate.
// An ID that gets past here is baked into a bucket name and a cache filename
// permanently, so it must be rejected before any resource is provisioned.
func TestOnboard_RejectsInvalidProjectID(t *testing.T) {
	for _, bad := range []string{"", "p", "../escape", "a/b", "Repo1", "a_b", "proj.11"} {
		r, k, b, c := &fakeRegistry{}, &fakeKMS{}, &fakeBackend{}, &fakeCreds{}
		err := newOnboarder(r, k, b, c).Onboard(context.Background(), bad)
		if err == nil {
			t.Errorf("Onboard(%q) should be rejected", bad)
			continue
		}
		if !errors.Is(err, project.ErrInvalidID) {
			t.Errorf("Onboard(%q): want ErrInvalidID, got %v", bad, err)
		}
		// Nothing may be provisioned for an ID that never passed validation.
		if r.registered || k.created || b.created || c.issued {
			t.Errorf("Onboard(%q) provisioned resources despite an invalid ID", bad)
		}
	}
}
