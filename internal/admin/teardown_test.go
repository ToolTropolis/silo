package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/tooltropolis/silo/internal/backend"
	"github.com/tooltropolis/silo/internal/registry"
)

// tdFakes records what each layer was asked to do, so tests can assert both
// ordering and that a step touched only its own layer.
type tdFakes struct {
	reg   *tdRegistry
	kms   *tdKMS
	be    *tdBackend
	creds *tdCreds
}

type tdRegistry struct {
	rec        registry.ProjectRecord
	getErr     error
	statuses   []string // every UpdateStatus, in order
	deregister int
}

func (r *tdRegistry) Register(context.Context, registry.ProjectRecord) error { return nil }
func (r *tdRegistry) Get(_ context.Context, _ string) (registry.ProjectRecord, error) {
	if r.getErr != nil {
		return registry.ProjectRecord{}, r.getErr
	}
	return r.rec, nil
}
func (r *tdRegistry) List(context.Context) ([]registry.ProjectRecord, error) { return nil, nil }
func (r *tdRegistry) UpdateStatus(_ context.Context, _, status string) error {
	r.statuses = append(r.statuses, status)
	r.rec.Status = status
	return nil
}
func (r *tdRegistry) Deregister(context.Context, string) error { r.deregister++; return nil }
func (r *tdRegistry) UpdateRefs(context.Context, string, string, string) error {
	return nil
}

type tdKMS struct {
	revoked []string
	err     error
}

func (k *tdKMS) CreateKey(context.Context, string) (string, error) { return "", nil }
func (k *tdKMS) GetKey(context.Context, string) ([]byte, error)    { return nil, nil }
func (k *tdKMS) RevokeKey(_ context.Context, keyID string) error {
	if k.err != nil {
		return k.err
	}
	k.revoked = append(k.revoked, keyID)
	return nil
}

// tdBackend embeds the interface so only the methods teardown uses need real
// bodies; anything else panics loudly rather than silently no-op'ing.
type tdBackend struct {
	deleted []string
	err     error
	backend.DurableBackend
}

func (b *tdBackend) DeleteBucket(_ context.Context, projectID string) error {
	if b.err != nil {
		return b.err
	}
	b.deleted = append(b.deleted, projectID)
	return nil
}

type tdCreds struct {
	revoked []string
	err     error
}

func (c *tdCreds) Issue(context.Context, string, string) (string, error) { return "", nil }
func (c *tdCreds) Revoke(_ context.Context, credID string) error {
	if c.err != nil {
		return c.err
	}
	c.revoked = append(c.revoked, credID)
	return nil
}

// newTeardownFixture builds an Onboarder over fakes for a project in the given
// status.
func newTeardownFixture(status string) (*Onboarder, *tdFakes) {
	reg := &tdRegistry{rec: registry.ProjectRecord{
		ProjectID:    "proj-11",
		BucketName:   "silo-proj-11",
		CredentialID: "cred-ref",
		KeyID:        "projects/proj-11",
		Status:       status,
	}}
	f := &tdFakes{reg: reg, kms: &tdKMS{}, be: &tdBackend{}, creds: &tdCreds{}}
	o := &Onboarder{Registry: reg, KMS: f.kms, Backend: f.be, Creds: f.creds}
	return o, f
}

func TestParseStep(t *testing.T) {
	for _, s := range []string{"revoke-credential", "revoke-key", "delete-bucket", "deregister"} {
		if _, err := ParseStep(s); err != nil {
			t.Errorf("ParseStep(%q): %v", s, err)
		}
	}
	if _, err := ParseStep("nuke-everything"); !errors.Is(err, ErrUnknownStep) {
		t.Errorf("unknown step: want ErrUnknownStep, got %v", err)
	}
	// A typo must not silently match a real step.
	if _, err := ParseStep("delete_bucket"); !errors.Is(err, ErrUnknownStep) {
		t.Errorf("typo step should be rejected, got %v", err)
	}
}

func TestIsIrreversible(t *testing.T) {
	if !IsIrreversible(StepDeleteBucket) {
		t.Error("delete-bucket must be flagged irreversible")
	}
	for _, s := range []TeardownStep{StepRevokeCredential, StepRevokeKey, StepDeregister} {
		if IsIrreversible(s) {
			t.Errorf("%q should not be flagged irreversible", s)
		}
	}
}

// TestTeardown_EnforcesOrder is the core safety property: a later step cannot
// run before its predecessors. Deleting a bucket while a live credential still
// points at it would leave that credential dangling.
func TestTeardown_EnforcesOrder(t *testing.T) {
	ctx := context.Background()

	// An active project may only take the first step.
	for _, step := range []TeardownStep{StepRevokeKey, StepDeleteBucket, StepDeregister} {
		o, f := newTeardownFixture(registry.StatusActive)
		err := o.Teardown(ctx, "proj-11", step)
		if !errors.Is(err, ErrOutOfOrder) {
			t.Errorf("step %q on an active project: want ErrOutOfOrder, got %v", step, err)
		}
		// Critically: nothing was destroyed.
		if len(f.be.deleted) != 0 || len(f.kms.revoked) != 0 || f.reg.deregister != 0 {
			t.Errorf("step %q ran despite being out of order", step)
		}
	}
}

func TestTeardown_RejectsAlreadyDecommissioned(t *testing.T) {
	o, f := newTeardownFixture(registry.StatusDecommissioned)
	err := o.Teardown(context.Background(), "proj-11", StepDeleteBucket)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("want ErrOutOfOrder, got %v", err)
	}
	if len(f.be.deleted) != 0 {
		t.Error("a decommissioned project's bucket must not be deleted again")
	}
}

func TestTeardown_RejectsRepeatedFirstStep(t *testing.T) {
	o, _ := newTeardownFixture(registry.StatusDecommissioning)
	err := o.Teardown(context.Background(), "proj-11", StepRevokeCredential)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("re-running the first step should be reported, got %v", err)
	}
}

// TestTeardown_FullSequence walks all four steps in order and asserts each one
// touched exactly its own layer, with the right status transitions.
func TestTeardown_FullSequence(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusActive)

	// 1. revoke-credential → status becomes decommissioning
	if err := o.Teardown(ctx, "proj-11", StepRevokeCredential); err != nil {
		t.Fatalf("revoke-credential: %v", err)
	}
	if len(f.creds.revoked) != 1 || f.creds.revoked[0] != "cred-ref" {
		t.Fatalf("credential not revoked: %v", f.creds.revoked)
	}
	if len(f.reg.statuses) != 1 || f.reg.statuses[0] != registry.StatusDecommissioning {
		t.Fatalf("status should move to decommissioning, got %v", f.reg.statuses)
	}
	// Nothing destroyed yet.
	if len(f.kms.revoked) != 0 || len(f.be.deleted) != 0 {
		t.Fatal("first step must not touch the key or bucket")
	}

	// 2. revoke-key
	if err := o.Teardown(ctx, "proj-11", StepRevokeKey); err != nil {
		t.Fatalf("revoke-key: %v", err)
	}
	if len(f.kms.revoked) != 1 || f.kms.revoked[0] != "projects/proj-11" {
		t.Fatalf("key not revoked: %v", f.kms.revoked)
	}
	if len(f.be.deleted) != 0 {
		t.Fatal("revoke-key must not delete the bucket")
	}

	// 3. delete-bucket (irreversible)
	if err := o.Teardown(ctx, "proj-11", StepDeleteBucket); err != nil {
		t.Fatalf("delete-bucket: %v", err)
	}
	if len(f.be.deleted) != 1 {
		t.Fatalf("bucket not deleted: %v", f.be.deleted)
	}
	if f.reg.deregister != 0 {
		t.Fatal("delete-bucket must not deregister")
	}

	// 4. deregister → decommissioned, record removed
	if err := o.Teardown(ctx, "proj-11", StepDeregister); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	if f.reg.deregister != 1 {
		t.Fatalf("record not deregistered (%d)", f.reg.deregister)
	}
	last := f.reg.statuses[len(f.reg.statuses)-1]
	if last != registry.StatusDecommissioned {
		t.Fatalf("final status should be decommissioned, got %q", last)
	}
}

// TestTeardown_StopsOnLayerFailure: a failing layer must surface, not be
// swallowed and allow the sequence to continue.
func TestTeardown_StopsOnLayerFailure(t *testing.T) {
	o, f := newTeardownFixture(registry.StatusDecommissioning)
	f.be.err = errors.New("backend unreachable")

	err := o.Teardown(context.Background(), "proj-11", StepDeleteBucket)
	if err == nil {
		t.Fatal("a failing delete must surface")
	}
	if f.reg.deregister != 0 {
		t.Error("a failed bucket delete must not deregister the project")
	}
}

func TestTeardown_ValidatesInput(t *testing.T) {
	o, _ := newTeardownFixture(registry.StatusActive)
	ctx := context.Background()

	if err := o.Teardown(ctx, "", StepRevokeCredential); err == nil {
		t.Error("empty projectID should error")
	}
	if err := o.Teardown(ctx, "proj-11", TeardownStep("bogus")); !errors.Is(err, ErrUnknownStep) {
		t.Errorf("bogus step: want ErrUnknownStep, got %v", err)
	}
}

func TestTeardown_MissingProject(t *testing.T) {
	o, f := newTeardownFixture(registry.StatusActive)
	f.reg.getErr = registry.ErrNotFound

	err := o.Teardown(context.Background(), "nope", StepRevokeCredential)
	if err == nil {
		t.Fatal("tearing down an unknown project should error")
	}
	if len(f.creds.revoked) != 0 {
		t.Error("nothing should be revoked for an unknown project")
	}
}
