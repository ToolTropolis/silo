// Package project defines what a valid project ID is.
//
// A projectID is not just a key: it is concatenated into an S3 bucket name
// ("silo-<id>") and into a local filename ("<id>.bbolt"). Both uses are
// unforgiving, and they have different rules, so the ID must satisfy the
// intersection of the two. Validating in one place keeps the storage layer, the
// cache, and onboarding from disagreeing about what they accept.
package project

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrInvalidID is returned for any ID that fails validation. Callers wrap it
// rather than replacing it, so errors.Is stays useful across layers.
var ErrInvalidID = errors.New("project: invalid project ID")

// MaxIDLen bounds the ID so "silo-"+id stays within S3's 63-byte bucket limit,
// with headroom left for any future prefix change.
const MaxIDLen = 56

// MinIDLen mirrors S3's 3-byte bucket minimum, measured on the bucket rather
// than the ID: "silo-" already satisfies it, but a one-character project is
// almost always a typo, and rejecting it costs nothing.
const MinIDLen = 3

// ValidateID reports whether id is safe to use as both a bucket name component
// and a filename.
//
// The rules are S3's bucket-naming rules, which happen to be strictly tighter
// than what a filename needs — lowercase alphanumerics and hyphens only, no
// dots, no path separators. That single charset is what makes traversal
// ("../etc") impossible rather than something we filter for separately.
//
// Uppercase is rejected rather than lowercased. Silently folding "Repo1" to
// "repo1" would let two distinct --tokens entries resolve to the same bucket
// while keeping separate cache files — a project-isolation failure that would
// look like data mysteriously appearing in the wrong silo.
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidID)
	}
	if len(id) < MinIDLen {
		return fmt.Errorf("%w: %q is shorter than %d characters", ErrInvalidID, id, MinIDLen)
	}
	if len(id) > MaxIDLen {
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidID, id, MaxIDLen)
	}

	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		case c >= 'A' && c <= 'Z':
			return fmt.Errorf("%w: %q contains uppercase; bucket names are lowercase, and folding case "+
				"would let two project IDs share one bucket", ErrInvalidID, id)
		default:
			return fmt.Errorf("%w: %q contains %q; only lowercase letters, digits, and hyphens are allowed",
				ErrInvalidID, id, string(c))
		}
	}

	if id[0] == '-' || id[len(id)-1] == '-' {
		return fmt.Errorf("%w: %q must start and end with a letter or digit", ErrInvalidID, id)
	}
	if strings.Contains(id, "--") {
		return fmt.Errorf("%w: %q contains consecutive hyphens", ErrInvalidID, id)
	}
	// S3 rejects bucket names that parse as IPv4 addresses.
	if net.ParseIP(id) != nil {
		return fmt.Errorf("%w: %q looks like an IP address", ErrInvalidID, id)
	}
	// Reserved by S3: xn-- is punycode, and these suffixes name access points.
	if strings.HasPrefix(id, "xn--") || strings.HasPrefix(id, "sthree-") {
		return fmt.Errorf("%w: %q uses a reserved prefix", ErrInvalidID, id)
	}
	if strings.HasSuffix(id, "-s3alias") || strings.HasSuffix(id, "--ol-s3") {
		return fmt.Errorf("%w: %q uses a reserved suffix", ErrInvalidID, id)
	}
	return nil
}
