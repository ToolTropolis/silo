package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrPreconditionMismatch is returned when a write carried a content hash that
// no longer matches what is stored.
//
// Distinct from ErrTooManyConflicts, which means the CAS loop kept losing races
// and retrying is reasonable: this means the caller's assumption about the
// current content was wrong, so retrying the same write would clobber whatever
// landed in between. The caller has to re-read and decide.
var ErrPreconditionMismatch = errors.New("daemon: content hash precondition failed")

// ContentHash returns the hash a caller round-trips to express "I am editing
// the version I last read".
//
// SHA-256 of the content rather than the backend's ETag: an ETag is
// S3-specific, is not a content hash for multipart uploads, and would leak the
// storage layer into the agent-facing API. A hash of the bytes means the same
// thing for a cached read, a durable read, and any future backend.
//
// The hash of absent content is the hash of no bytes, so a caller can express
// "create this only if nothing exists yet" without a separate flag.
func ContentHash(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// checkPrecondition compares the stored content against the hash a caller
// expected, before anything is written.
//
// An empty expected hash disables the check, so an unconditional write behaves
// exactly as it did.
func checkPrecondition(expected string, current []byte) error {
	if expected == "" {
		return nil
	}
	if actual := ContentHash(current); actual != expected {
		return fmt.Errorf("%w: expected %s but the stored content hashes to %s — "+
			"re-read the path and reapply your change",
			ErrPreconditionMismatch, short(expected), short(actual))
	}
	return nil
}

// short trims a hash for an error message. The full 64 hex characters make the
// message unreadable, and the first 12 are enough to see that two hashes differ.
func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
