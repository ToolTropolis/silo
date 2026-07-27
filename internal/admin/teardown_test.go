package admin

import (
	"context"
	"errors"
	"strings"
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
func (r *tdRegistry) SetRepo(context.Context, string, string, string) error { return nil }

func (r *tdRegistry) Deregister(context.Context, string) error { r.deregister++; return nil }

// UpdateRefs and ClearBucket mutate the stored record the way rqlite does —
// teardown derives its ordering from these fields, so a fake that ignored the
// writes would let out-of-order steps pass in tests but fail in production.
func (r *tdRegistry) UpdateRefs(_ context.Context, _, keyID, credentialID string) error {
	r.rec.KeyID, r.rec.CredentialID = keyID, credentialID
	return nil
}
func (r *tdRegistry) ClearBucket(context.Context, string) error {
	r.rec.BucketName = ""
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
//
// Refs are set to match the status, because ordering is derived from which refs
// remain: a "decommissioning" project has already had its credential revoked,
// so leaving CredentialID populated would describe a state that cannot occur.
func newTeardownFixture(status string) (*Onboarder, *tdFakes) {
	rec := registry.ProjectRecord{
		ProjectID:    "proj-11",
		BucketName:   "silo-proj-11",
		CredentialID: "cred-ref",
		KeyID:        "projects/proj-11",
		Status:       status,
	}
	switch status {
	case registry.StatusDecommissioning:
		rec.CredentialID = "" // step 1 done; revoke-key is next
	case registry.StatusDecommissioned:
		rec.CredentialID, rec.KeyID, rec.BucketName = "", "", ""
	}
	reg := &tdRegistry{rec: rec}
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

// TestTeardown_DeregisterRequiresBucketDeleted is the regression test for the
// orphaned-bucket bug.
//
// Status has three values for four steps, so steps 2-4 all sat in
// "decommissioning" and were indistinguishable. deregister therefore ran while
// the bucket was still live, deleting the registry record that named it — and
// since delete-bucket loads that record to find the bucket, the data became
// unreachable through siloctl entirely. Unrecoverable, not merely out of order.
func TestTeardown_DeregisterRequiresBucketDeleted(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusDecommissioning) // key + bucket still live

	err := o.Teardown(ctx, "proj-11", StepDeregister)
	if !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("deregister with a live bucket must be refused, got %v", err)
	}
	if f.reg.deregister != 0 {
		t.Fatal("the registry record was deleted while the bucket was still live — the bucket is now orphaned")
	}

	// The record must still name the bucket, so teardown can be resumed.
	if f.reg.rec.BucketName == "" {
		t.Fatal("bucket name lost from the record; teardown could no longer find the bucket")
	}
}

// TestTeardown_StepsAreNotReplayable: every step is refused once its own work is
// done, so a repeated invocation can't double-revoke or re-delete.
func TestTeardown_StepsAreNotReplayable(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusActive)

	for _, step := range TeardownOrder {
		if err := o.Teardown(ctx, "proj-11", step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if err := o.Teardown(ctx, "proj-11", step); !errors.Is(err, ErrOutOfOrder) {
			t.Errorf("re-running %q should be refused, got %v", step, err)
		}
	}

	if len(f.creds.revoked) != 1 || len(f.kms.revoked) != 1 || len(f.be.deleted) != 1 || f.reg.deregister != 1 {
		t.Errorf("each layer must be touched exactly once: creds=%d kms=%d buckets=%d deregister=%d",
			len(f.creds.revoked), len(f.kms.revoked), len(f.be.deleted), f.reg.deregister)
	}
}

// TestTeardown_FailedStepStaysPending: when a layer fails, its ref must survive
// so the step is retried rather than silently skipped.
func TestTeardown_FailedStepStaysPending(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusDecommissioning)
	f.kms.err = errors.New("vault unreachable")

	if err := o.Teardown(ctx, "proj-11", StepRevokeKey); err == nil {
		t.Fatal("a failing key revoke must surface")
	}
	if f.reg.rec.KeyID == "" {
		t.Fatal("key ref cleared despite the revoke failing — the step would be skipped")
	}

	// Still due, and the next step is still blocked.
	if got := nextStep(f.reg.rec); got != StepRevokeKey {
		t.Errorf("want revoke-key still pending, got %q", got)
	}
	if err := o.Teardown(ctx, "proj-11", StepDeleteBucket); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("delete-bucket must stay blocked after a failed revoke, got %v", err)
	}

	// Retry succeeds once the layer recovers.
	f.kms.err = nil
	if err := o.Teardown(ctx, "proj-11", StepRevokeKey); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
}

// TestNextStep_ReportsTruePosition: the CLI prints "next" from this, so it must
// reflect real state rather than which step was last invoked.
func TestNextStep_ReportsTruePosition(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusActive)

	for _, want := range TeardownOrder {
		got, err := NextStep(ctx, f.reg, "proj-11")
		if err != nil {
			t.Fatalf("NextStep: %v", err)
		}
		if got != want {
			t.Fatalf("want next=%q, got %q", want, got)
		}
		if err := o.Teardown(ctx, "proj-11", want); err != nil {
			t.Fatalf("%s: %v", want, err)
		}
	}

	// A deregistered project is gone from the registry: complete, not an error.
	f.reg.getErr = registry.ErrNotFound
	got, err := NextStep(ctx, f.reg, "proj-11")
	if err != nil {
		t.Fatalf("a missing record means fully torn down, got error %v", err)
	}
	if got != "" {
		t.Errorf("want no pending step, got %q", got)
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

// recordingPurger captures cache-purge calls so teardown ordering can be
// asserted.
type recordingPurger struct {
	purged []string
	err    error
}

func (p *recordingPurger) PurgeCache(_ context.Context, projectID string) error {
	if p.err != nil {
		return p.err
	}
	p.purged = append(p.purged, projectID)
	return nil
}

// TestTeardown_PurgesCacheWithTheBucket: the cached copy goes when the bucket
// does. Deferring it to deregister would leave a project's memory in plaintext
// on local disk through an operator-paced gap, with nothing left upstream for
// it to be consistent with.
func TestTeardown_PurgesCacheWithTheBucket(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusDecommissioning)
	purger := &recordingPurger{}
	o.Cache = purger

	// Advance to delete-bucket.
	if err := o.Teardown(ctx, "proj-11", StepRevokeKey); err != nil {
		t.Fatalf("revoke-key: %v", err)
	}
	if len(purger.purged) != 0 {
		t.Fatal("the cache must not be purged before the bucket is destroyed")
	}

	if err := o.Teardown(ctx, "proj-11", StepDeleteBucket); err != nil {
		t.Fatalf("delete-bucket: %v", err)
	}
	if len(f.be.deleted) != 1 {
		t.Fatalf("bucket not deleted: %v", f.be.deleted)
	}
	if len(purger.purged) != 1 || purger.purged[0] != "proj-11" {
		t.Errorf("cache purged = %v, want [proj-11] alongside the bucket", purger.purged)
	}
}

// A purge failure must stop teardown rather than be swallowed — the daemon
// refuses while writes are queued, and that refusal is the whole point.
func TestTeardown_PurgeFailureStopsTeardown(t *testing.T) {
	ctx := context.Background()
	o, _ := newTeardownFixture(registry.StatusDecommissioning)
	o.Cache = &recordingPurger{err: errors.New("2 write(s) are still queued")}

	if err := o.Teardown(ctx, "proj-11", StepRevokeKey); err != nil {
		t.Fatalf("revoke-key: %v", err)
	}
	err := o.Teardown(ctx, "proj-11", StepDeleteBucket)
	if err == nil {
		t.Fatal("a failed cache purge must surface, not be swallowed")
	}
	if !strings.Contains(err.Error(), "purge cache") {
		t.Errorf("error should name the purge, got: %v", err)
	}
}

// TestTeardown_RefusedPurgeLeavesTheBucket is the ordering guarantee.
//
// The daemon refuses to purge while writes are still queued. If the bucket were
// deleted first, those writes would be addressed to a destination that no
// longer exists — unsyncable, and lost whenever the cache is finally cleared.
// The refusal has to stop teardown while it can still do some good.
func TestTeardown_RefusedPurgeLeavesTheBucket(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusDecommissioning)
	o.Cache = &recordingPurger{err: errors.New("1 write(s) are still queued")}

	if err := o.Teardown(ctx, "proj-11", StepRevokeKey); err != nil {
		t.Fatalf("revoke-key: %v", err)
	}
	if err := o.Teardown(ctx, "proj-11", StepDeleteBucket); err == nil {
		t.Fatal("a refused purge must stop the step")
	}
	if len(f.be.deleted) != 0 {
		t.Error("the bucket must survive a refused purge, or the queued writes " +
			"have nowhere left to sync to")
	}
}

// Teardown must still work on a host with no daemon — it just says the local
// copy remains rather than pretending it is gone.
func TestTeardown_WithoutAPurgerStillCompletes(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusDecommissioning)
	o.Cache = nil

	if err := o.Teardown(ctx, "proj-11", StepRevokeKey); err != nil {
		t.Fatalf("revoke-key: %v", err)
	}
	if err := o.Teardown(ctx, "proj-11", StepDeleteBucket); err != nil {
		t.Fatalf("delete-bucket without a purger should still work: %v", err)
	}
	if len(f.be.deleted) != 1 {
		t.Error("the bucket must still be deleted")
	}
}

// recordingSettings notes which projects had their stored cache policy removed.
type recordingSettings struct {
	deleted []string
	err     error
}

func (r *recordingSettings) DeleteSettings(_ context.Context, projectID string) error {
	if r.err != nil {
		return r.err
	}
	r.deleted = append(r.deleted, projectID)
	return nil
}

// A project's cache policy must not outlive the project. Left behind, a later
// project re-onboarded under the same ID silently inherits the previous
// tenant's retention settings — invisible, and the same shape of mistake as
// inheriting its cached memory.
func TestTeardown_DeregisterRemovesStoredSettings(t *testing.T) {
	ctx := context.Background()
	o, _ := newTeardownFixture(registry.StatusDecommissioning)
	settings := &recordingSettings{}
	o.Settings = settings

	for _, step := range []TeardownStep{StepRevokeKey, StepDeleteBucket, StepDeregister} {
		if err := o.Teardown(ctx, "proj-11", step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	if len(settings.deleted) != 1 || settings.deleted[0] != "proj-11" {
		t.Errorf("deleted settings for %v, want [proj-11] — a stale policy row would be "+
			"inherited by the next project with this ID", settings.deleted)
	}
}

// Settings are removed at deregister, not earlier: a teardown paused midway
// should still show the policy that was in force.
func TestTeardown_SettingsSurviveUntilDeregister(t *testing.T) {
	ctx := context.Background()
	o, _ := newTeardownFixture(registry.StatusDecommissioning)
	settings := &recordingSettings{}
	o.Settings = settings

	for _, step := range []TeardownStep{StepRevokeKey, StepDeleteBucket} {
		if err := o.Teardown(ctx, "proj-11", step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if len(settings.deleted) != 0 {
		t.Errorf("settings removed at %v, want them kept until deregister", settings.deleted)
	}
}

// A settings store that fails must not block the deregister that removes the
// record: a stale policy row is untidy, an un-deregisterable project is not.
func TestTeardown_SettingsFailureDoesNotBlockDeregister(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusDecommissioning)
	o.Settings = &recordingSettings{err: errors.New("rqlite unreachable")}

	for _, step := range []TeardownStep{StepRevokeKey, StepDeleteBucket, StepDeregister} {
		if err := o.Teardown(ctx, "proj-11", step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if f.reg.deregister == 0 {
		t.Error("deregister must still complete when the settings store is unreachable")
	}
}

// A nil store is the pre-console configuration and must keep working.
func TestTeardown_NilSettingsStoreIsFine(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusDecommissioning)
	o.Settings = nil

	for _, step := range []TeardownStep{StepRevokeKey, StepDeleteBucket, StepDeregister} {
		if err := o.Teardown(ctx, "proj-11", step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if f.reg.deregister == 0 {
		t.Error("teardown must complete with no settings store configured")
	}
}

// recordingRevoker notes which projects had their agent tokens revoked.
type recordingRevoker struct {
	revoked []string
	count   int
	err     error
}

func (r *recordingRevoker) RevokeProjectTokens(_ context.Context, projectID string) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	r.revoked = append(r.revoked, projectID)
	return r.count, nil
}

// Agent tokens must die at revoke-credential — the step whose purpose is
// removing access — not at deregister. Leaving them live until the last step
// would let an agent keep reading and writing through the operator-paced gap
// while the bucket is being deleted underneath it.
func TestTeardown_RevokesAgentTokensAtRevokeCredential(t *testing.T) {
	ctx := context.Background()
	o, _ := newTeardownFixture(registry.StatusActive)
	tokens := &recordingRevoker{count: 2}
	o.Tokens = tokens

	if err := o.Teardown(ctx, "proj-11", StepRevokeCredential); err != nil {
		t.Fatalf("revoke-credential: %v", err)
	}
	if len(tokens.revoked) != 1 || tokens.revoked[0] != "proj-11" {
		t.Errorf("revoked = %v, want [proj-11] at the first teardown step", tokens.revoked)
	}
}

// A failure to revoke tokens must stop the step. Proceeding would revoke the S3
// credential while leaving live tokens pointed at a project being destroyed.
func TestTeardown_TokenRevocationFailureStopsTheStep(t *testing.T) {
	ctx := context.Background()
	o, f := newTeardownFixture(registry.StatusActive)
	o.Tokens = &recordingRevoker{err: errors.New("rqlite unreachable")}

	if err := o.Teardown(ctx, "proj-11", StepRevokeCredential); err == nil {
		t.Fatal("a failed token revocation must stop the step")
	}
	if len(f.creds.revoked) != 0 {
		t.Error("the S3 credential must not be revoked when tokens could not be")
	}
}

// A nil token store is a real gap, not a nicety: teardown proceeds but must say
// so, because those tokens keep authorizing against a project that is going away.
func TestTeardown_NoTokenStoreStillCompletes(t *testing.T) {
	ctx := context.Background()
	o, _ := newTeardownFixture(registry.StatusActive)
	o.Tokens = nil

	if err := o.Teardown(ctx, "proj-11", StepRevokeCredential); err != nil {
		t.Fatalf("teardown should still complete: %v", err)
	}
}
