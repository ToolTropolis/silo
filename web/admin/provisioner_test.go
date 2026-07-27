package admin

import (
	"context"
	"testing"

	adminpkg "github.com/tooltropolis/silo/internal/admin"
	"github.com/tooltropolis/silo/internal/registry"
)

// planRegistry is a minimal TenantRegistry holding one record, enough for
// NextStep to derive teardown progress from its refs.
type planRegistry struct {
	rec registry.ProjectRecord
	err error
}

func (p *planRegistry) Register(context.Context, registry.ProjectRecord) error { return nil }
func (p *planRegistry) Get(context.Context, string) (registry.ProjectRecord, error) {
	return p.rec, p.err
}
func (p *planRegistry) List(context.Context) ([]registry.ProjectRecord, error)   { return nil, nil }
func (p *planRegistry) UpdateStatus(context.Context, string, string) error       { return nil }
func (p *planRegistry) UpdateRefs(context.Context, string, string, string) error { return nil }
func (p *planRegistry) ClearBucket(context.Context, string) error                { return nil }
func (p *planRegistry) SetRepo(context.Context, string, string, string) error    { return nil }
func (p *planRegistry) Deregister(context.Context, string) error                 { return nil }

// Progress is derived from the record's own refs, the same way internal/admin
// derives it — each step clears the ref it consumed, so a cleared ref is the
// evidence that step ran.
func TestTeardownPlan_DerivesProgressFromRefs(t *testing.T) {
	tests := []struct {
		name     string
		rec      registry.ProjectRecord
		wantDone []string
		wantNext string
	}{
		{
			name: "a fresh project has everything pending",
			rec: registry.ProjectRecord{
				ProjectID: "p", BucketName: "silo-p", CredentialID: "c", KeyID: "k",
				Status: registry.StatusActive,
			},
			wantDone: nil,
			wantNext: "revoke-credential",
		},
		{
			name: "a cleared credential means step 1 is done",
			rec: registry.ProjectRecord{
				ProjectID: "p", BucketName: "silo-p", KeyID: "k",
				Status: registry.StatusDecommissioning,
			},
			wantDone: []string{"revoke-credential"},
			wantNext: "revoke-key",
		},
		{
			name: "a cleared key means steps 1-2 are done",
			rec: registry.ProjectRecord{
				ProjectID: "p", BucketName: "silo-p",
				Status: registry.StatusDecommissioning,
			},
			wantDone: []string{"revoke-credential", "revoke-key"},
			wantNext: "delete-bucket",
		},
		{
			name: "a cleared bucket leaves only deregister",
			rec: registry.ProjectRecord{
				ProjectID: "p", Status: registry.StatusDecommissioning,
			},
			wantDone: []string{"revoke-credential", "revoke-key", "delete-bucket"},
			wantNext: "deregister",
		},
		{
			name: "a decommissioned project has everything done",
			rec: registry.ProjectRecord{
				ProjectID: "p", Status: registry.StatusDecommissioned,
			},
			wantDone: []string{"revoke-credential", "revoke-key", "delete-bucket", "deregister"},
			wantNext: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := &planRegistry{rec: tc.rec}
			p := &OnboarderProvisioner{Onboarder: &adminpkg.Onboarder{Registry: reg}, Registry: reg}

			steps, err := p.TeardownPlan(context.Background(), "p")
			if err != nil {
				t.Fatalf("TeardownPlan: %v", err)
			}
			if len(steps) != 4 {
				t.Fatalf("got %d steps, want all 4 always listed", len(steps))
			}

			var done []string
			var next string
			for _, s := range steps {
				if s.Done {
					done = append(done, s.Name)
				} else if next == "" {
					next = s.Name
				}
				if s.Description == "" {
					t.Errorf("step %q has no description", s.Name)
				}
			}
			if next != tc.wantNext {
				t.Errorf("next = %q, want %q", next, tc.wantNext)
			}
			if len(done) != len(tc.wantDone) {
				t.Errorf("done = %v, want %v", done, tc.wantDone)
			}
			for i := range tc.wantDone {
				if i < len(done) && done[i] != tc.wantDone[i] {
					t.Errorf("done[%d] = %q, want %q", i, done[i], tc.wantDone[i])
				}
			}
		})
	}
}

// A project already gone from the registry is complete, not an error.
func TestTeardownPlan_MissingProjectIsNotAnError(t *testing.T) {
	reg := &planRegistry{err: registry.ErrNotFound}
	p := &OnboarderProvisioner{Onboarder: &adminpkg.Onboarder{Registry: reg}, Registry: reg}

	steps, err := p.TeardownPlan(context.Background(), "gone")
	if err != nil {
		t.Fatalf("a deregistered project should not error: %v", err)
	}
	// NextStep reports "" for an absent record, so every step reads as done.
	for _, s := range steps {
		if !s.Done {
			t.Errorf("step %q should be done for an absent project", s.Name)
		}
	}
}
