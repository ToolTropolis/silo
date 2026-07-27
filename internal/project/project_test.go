package project

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		ok   bool
	}{
		{"simple", "repo1", true},
		{"with digits", "project2", true},
		{"hyphenated", "nj-agents", true},
		{"multi hyphen", "a-b-c", true},
		{"min length", "abc", true},
		{"max length", strings.Repeat("a", MaxIDLen), true},

		{"empty", "", false},
		{"too short", "ab", false},
		{"too long", strings.Repeat("a", MaxIDLen+1), false},

		// Traversal and separators — the reason this package exists. The ID
		// becomes a filename, so any of these could escape the cache directory.
		{"parent traversal", "../etc/silo", false},
		{"bare parent", "..", false},
		{"slash", "a/b", false},
		{"backslash", `a\b`, false},
		{"leading slash", "/abs", false},
		{"null byte", "a\x00b", false},
		{"newline", "a\nb", false},

		// Case folding would let two IDs share one bucket.
		{"uppercase", "Repo1", false},
		{"all caps", "REPO", false},

		{"underscore", "a_b", false},
		{"dot", "a.b", false},
		{"space", "a b", false},
		{"leading hyphen", "-repo", false},
		{"trailing hyphen", "repo-", false},
		{"consecutive hyphens", "a--b", false},
		{"ipv4", "192.168.1.1", false},
		{"punycode prefix", "xn--abc", false},
		{"s3alias suffix", "my-bucket-s3alias", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateID(tc.id)
			if tc.ok && err != nil {
				t.Errorf("ValidateID(%q) = %v, want nil", tc.id, err)
			}
			if !tc.ok {
				if err == nil {
					t.Errorf("ValidateID(%q) = nil, want an error", tc.id)
				} else if !errors.Is(err, ErrInvalidID) {
					t.Errorf("ValidateID(%q) error should wrap ErrInvalidID, got %v", tc.id, err)
				}
			}
		})
	}
}

// The existing dev-stack and documented project IDs must keep working — this
// change is meant to be invisible to anyone already using Silo correctly.
func TestValidateID_ExistingProjectsStillValid(t *testing.T) {
	for _, id := range []string{"demo", "quickstart", "tdfix", "nj-agents"} {
		if err := ValidateID(id); err != nil {
			t.Errorf("existing project %q should remain valid: %v", id, err)
		}
	}
}
