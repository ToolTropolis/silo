package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tooltropolis/silo/internal/daemon"
	"github.com/tooltropolis/silo/internal/registry"
)

func TestParseTokens(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want map[string]string
		ok   bool
	}{
		{"single", "tok=repo1", map[string]string{"tok": "repo1"}, true},
		{"multiple", "a=repo1,b=project2", map[string]string{"a": "repo1", "b": "project2"}, true},
		{"several agents, one project", "a=repo1,b=repo1", map[string]string{"a": "repo1", "b": "repo1"}, true},
		{"whitespace tolerated", " a=repo1 , b=project2 ", map[string]string{"a": "repo1", "b": "project2"}, true},

		{"empty", "", nil, false},
		{"missing separator", "justatoken", nil, false},
		{"empty token", "=repo1", nil, false},
		{"empty project", "tok=", nil, false},
		// A bad project ID must fail at startup rather than on the first write.
		{"invalid project id", "tok=Repo_1", nil, false},
		{"traversal project id", "tok=../escape", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTokens(tc.spec)
			if tc.ok {
				if err != nil {
					t.Fatalf("parseTokens(%q) = %v, want success", tc.spec, err)
				}
				if len(got) != len(tc.want) {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
				for token, project := range tc.want {
					if got[token] != project {
						t.Errorf("token %q maps to %q, want %q", token, got[token], project)
					}
				}
				return
			}
			if err == nil {
				t.Errorf("parseTokens(%q) should have failed", tc.spec)
			}
		})
	}
}

// listRegistry is a minimal TenantRegistry that only supports List.
type listRegistry struct {
	registry.TenantRegistry
	recs []registry.ProjectRecord
	err  error
}

func (r *listRegistry) List(context.Context) ([]registry.ProjectRecord, error) {
	return r.recs, r.err
}

// TestSyncProjects: the sync worker's project list is the union of the token
// set and the registry. Each source covers a gap the other has.
func TestSyncProjects(t *testing.T) {
	verifier := daemon.StaticTokenVerifier{"a": "repo1", "b": "repo1", "c": "project2"}

	t.Run("union of tokens and registry", func(t *testing.T) {
		reg := &listRegistry{recs: []registry.ProjectRecord{
			{ProjectID: "repo1", Status: registry.StatusActive},
			// Registered but has no token — e.g. rotated away mid-outage. Its
			// queue must still drain, which is the whole point of consulting
			// the registry.
			{ProjectID: "orphaned", Status: registry.StatusActive},
		}}
		got := syncProjects(context.Background(), reg, verifier)
		want := []string{"orphaned", "project2", "repo1"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("decommissioned projects are skipped", func(t *testing.T) {
		reg := &listRegistry{recs: []registry.ProjectRecord{
			{ProjectID: "gone", Status: registry.StatusDecommissioned},
		}}
		for _, id := range syncProjects(context.Background(), reg, verifier) {
			if id == "gone" {
				t.Error("a decommissioned project has no bucket to drain into")
			}
		}
	})

	t.Run("registry failure falls back to the token set", func(t *testing.T) {
		reg := &listRegistry{err: errors.New("rqlite unreachable")}
		got := syncProjects(context.Background(), reg, verifier)
		want := []string{"project2", "repo1"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v — a registry blip must not stop syncing known projects", got, want)
		}
	})

	t.Run("no registry configured", func(t *testing.T) {
		got := syncProjects(context.Background(), nil, verifier)
		want := []string{"project2", "repo1"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}
