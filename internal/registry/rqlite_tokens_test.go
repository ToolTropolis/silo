package registry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rqlite/gorqlite"
)

// uniqueTokenProject returns a run-unique project ID and revokes its tokens
// afterwards, so repeated runs do not accumulate rows.
func uniqueTokenProject(t *testing.T, r *Rqlite) string {
	t.Helper()
	id := fmt.Sprintf("tok-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = r.RevokeProjectTokens(context.Background(), id) })
	return id
}

func TestTokens_MintAndVerify(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueTokenProject(t, r)

	raw, err := r.MintToken(ctx, proj, "laptop", "nav", false)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if !LooksLikeToken(raw) {
		t.Errorf("minted token %q does not look like a Silo token", raw)
	}

	got, err := r.VerifyToken(ctx, raw)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if got.ProjectID != proj {
		t.Errorf("token resolved to %q, want %q", got.ProjectID, proj)
	}
	if got.ReadOnly {
		t.Error("a token minted read-write must not verify as read-only")
	}
}

// The security property the whole schema exists for: what is stored must not be
// usable as a credential. A leaked registry dump must not hand anyone access.
func TestTokens_RawTokenIsNotRecoverableFromStorage(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueTokenProject(t, r)

	raw, err := r.MintToken(ctx, proj, "ci", "nav", false)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	tokens, err := r.ListTokens(ctx, proj)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens, want 1", len(tokens))
	}

	stored := tokens[0]
	if stored.Hash == raw {
		t.Fatal("the raw token is stored verbatim — a registry dump would leak working credentials")
	}
	// Nothing on the record may contain the token, including in a field that
	// looks harmless like the label.
	for _, field := range []string{stored.Hash, stored.Label, stored.CreatedBy, stored.ProjectID} {
		if field != "" && len(raw) > 12 && contains(field, raw[len(TokenPrefix):]) {
			t.Errorf("stored field %q contains the raw token", field)
		}
	}
	if stored.Hash != HashToken(raw) {
		t.Error("stored hash does not match the token's hash; verification would fail")
	}
}

func TestTokens_UnknownTokenIsRejected(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()

	unknown, _ := NewRawToken()
	if _, err := r.VerifyToken(ctx, unknown); !errors.Is(err, ErrNotFound) {
		t.Errorf("an unminted token should be rejected, got %v", err)
	}
	if _, err := r.VerifyToken(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("an empty token should be rejected, got %v", err)
	}
}

// Revocation is what an operator reaches for in an incident, so it must be
// immediate and total.
func TestTokens_RevokedTokenStopsWorking(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueTokenProject(t, r)

	raw, err := r.MintToken(ctx, proj, "leaked", "nav", false)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if _, err := r.VerifyToken(ctx, raw); err != nil {
		t.Fatalf("token should work before revocation: %v", err)
	}

	if err := r.RevokeToken(ctx, HashToken(raw)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	// A revoked token must be indistinguishable from an unknown one: saying
	// which would confirm to an attacker that a token was once valid.
	if _, err := r.VerifyToken(ctx, raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked token must be rejected, got %v", err)
	}

	// The row survives so an audit can still answer what happened to it.
	tokens, _ := r.ListTokens(ctx, proj)
	if len(tokens) != 1 || !tokens[0].Revoked() {
		t.Errorf("the revoked token should remain listed and marked, got %+v", tokens)
	}
}

// Revocation is idempotent because it is used under pressure.
func TestTokens_RevokeIsIdempotent(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueTokenProject(t, r)

	raw, _ := r.MintToken(ctx, proj, "x", "nav", false)
	hash := HashToken(raw)

	for i := range 3 {
		if err := r.RevokeToken(ctx, hash); err != nil {
			t.Errorf("revoke #%d failed: %v", i+1, err)
		}
	}
	if err := r.RevokeToken(ctx, HashToken("never-minted")); err != nil {
		t.Errorf("revoking an unknown token should be a no-op, got %v", err)
	}
}

// Several tokens per project is the point of labels: one can be revoked
// without disturbing the others.
func TestTokens_RevokingOneLeavesTheOthers(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueTokenProject(t, r)

	laptop, _ := r.MintToken(ctx, proj, "laptop", "nav", false)
	ci, _ := r.MintToken(ctx, proj, "ci", "nav", false)

	if err := r.RevokeToken(ctx, HashToken(laptop)); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	if _, err := r.VerifyToken(ctx, laptop); !errors.Is(err, ErrNotFound) {
		t.Error("the revoked token should be dead")
	}
	if got, err := r.VerifyToken(ctx, ci); err != nil || got.ProjectID != proj {
		t.Errorf("the other token should still work, got %q / %v", got.ProjectID, err)
	}
}

// Teardown uses this: a decommissioned project's credentials must not outlive it.
func TestTokens_RevokeProjectTokens(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueTokenProject(t, r)

	a, _ := r.MintToken(ctx, proj, "one", "nav", false)
	b, _ := r.MintToken(ctx, proj, "two", "nav", false)

	n, err := r.RevokeProjectTokens(ctx, proj)
	if err != nil {
		t.Fatalf("RevokeProjectTokens: %v", err)
	}
	if n != 2 {
		t.Errorf("revoked %d tokens, want 2", n)
	}
	for _, tok := range []string{a, b} {
		if _, err := r.VerifyToken(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Error("every token for the project should be dead")
		}
	}
}

// A token is scoped to exactly one project — that is the daemon's whole
// authorization boundary.
func TestTokens_ScopedToOneProject(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	projA := uniqueTokenProject(t, r)
	projB := uniqueTokenProject(t, r)

	tokA, _ := r.MintToken(ctx, projA, "a", "nav", false)

	got, err := r.VerifyToken(ctx, tokA)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if got.ProjectID == projB {
		t.Fatal("ISOLATION FAILURE: a token resolved to the wrong project")
	}
	if got.ProjectID != projA {
		t.Errorf("token resolved to %q, want %q", got.ProjectID, projA)
	}

	// B's token list must not include A's token.
	tokens, _ := r.ListTokens(ctx, projB)
	for _, tok := range tokens {
		if tok.Hash == HashToken(tokA) {
			t.Error("ISOLATION FAILURE: project B can see project A's token")
		}
	}
}

func TestTokens_MintRejectsInvalidProject(t *testing.T) {
	r := newLiveRegistry(t)
	for _, bad := range []string{"", "ab", "Repo1", "my_project", "../escape"} {
		if _, err := r.MintToken(context.Background(), bad, "x", "nav", false); err == nil {
			t.Errorf("minting for %q should be rejected", bad)
		}
	}
}

func TestTokens_TouchRecordsLastUse(t *testing.T) {
	r := newLiveRegistry(t)
	ctx := context.Background()
	proj := uniqueTokenProject(t, r)

	raw, _ := r.MintToken(ctx, proj, "x", "nav", false)
	before, _ := r.ListTokens(ctx, proj)
	if before[0].LastUsedAt != "" {
		t.Error("a fresh token should have no last-used timestamp")
	}

	if err := r.TouchToken(ctx, HashToken(raw)); err != nil {
		t.Fatalf("TouchToken: %v", err)
	}
	after, _ := r.ListTokens(ctx, proj)
	if after[0].LastUsedAt == "" {
		t.Error("last-used should be recorded, so an unused token is identifiable")
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

// The scope has to survive the round trip through rqlite. A flag that is stored
// but read back wrong is worse than no flag: the console would show read-only
// while the daemon allowed writes.
func TestTokens_ReadOnlyScopeRoundTrips(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)
	proj := uniqueTokenProject(t, r)

	ro, err := r.MintToken(ctx, proj, "reader", "nav", true)
	if err != nil {
		t.Fatalf("MintToken read-only: %v", err)
	}
	rw, err := r.MintToken(ctx, proj, "writer", "nav", false)
	if err != nil {
		t.Fatalf("MintToken read-write: %v", err)
	}

	gotRO, err := r.VerifyToken(ctx, ro)
	if err != nil {
		t.Fatalf("VerifyToken read-only: %v", err)
	}
	if !gotRO.ReadOnly {
		t.Error("a token minted read-only verified as read-write — the daemon would allow writes")
	}
	if gotRO.ProjectID != proj {
		t.Errorf("read-only token resolved to %q, want %q", gotRO.ProjectID, proj)
	}

	gotRW, err := r.VerifyToken(ctx, rw)
	if err != nil {
		t.Fatalf("VerifyToken read-write: %v", err)
	}
	if gotRW.ReadOnly {
		t.Error("a token minted read-write verified as read-only — working agents would break")
	}

	// ListTokens is what the console renders, so it must agree with what
	// VerifyToken enforces. Disagreement here is a UI that lies.
	list, err := r.ListTokens(ctx, proj)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	byLabel := map[string]AgentToken{}
	for _, tok := range list {
		byLabel[tok.Label] = tok
	}
	if !byLabel["reader"].ReadOnly {
		t.Error("ListTokens reports the read-only token as read-write")
	}
	if byLabel["writer"].ReadOnly {
		t.Error("ListTokens reports the read-write token as read-only")
	}
}

// Tokens issued before 009 existed were read-write and must stay that way:
// refusing their writes on upgrade would break every agent already running.
//
// The guarantee is structural rather than defensive. 009 adds the column as
// NOT NULL DEFAULT 0, so the ALTER backfills every existing row to 0 and the
// constraint makes a NULL unstorable afterwards — asserted here, because if a
// later migration relaxed it the read path would start seeing a value it has
// no schema-level reason to expect.
func TestTokens_PreMigrationTokensRemainWritable(t *testing.T) {
	ctx := context.Background()
	r := newLiveRegistry(t)
	proj := uniqueTokenProject(t, r)

	raw, err := r.MintToken(ctx, proj, "legacy", "nav", false)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	// A NULL is what a pre-009 row would look like if the column were nullable.
	// The constraint must refuse it, which is what makes every existing token
	// definitively read-write after the upgrade.
	_, err = r.conn.WriteOneParameterizedContext(ctx, gorqlite.ParameterizedStatement{
		Query:     `UPDATE agent_tokens SET read_only = NULL WHERE token_hash = ?`,
		Arguments: []interface{}{HashToken(raw)},
	})
	if err == nil {
		t.Error("read_only accepted NULL — a pre-migration row could then be " +
			"read as an unrecognized value rather than as read-write")
	}

	// And the token still writes, which is the behaviour agents depend on.
	got, err := r.VerifyToken(ctx, raw)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if got.ReadOnly {
		t.Error("an existing token became read-only — upgrading would break running agents")
	}
}

// readsOnly guards the read path against a value the schema does not currently
// permit but a driver change could produce. Unit-level because the NOT NULL
// constraint makes most of these unreachable through SQL.
func TestReadsOnly_FailsClosedOnUnexpectedTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want bool
	}{
		// NULL means a row written before the column existed. That token WAS
		// read-write, so this is the one unrecognized-ish case that must not
		// deny.
		{"nil is read-write", nil, false},
		{"zero is read-write", float64(0), false},
		{"one is read-only", float64(1), true},
		{"int64 zero", int64(0), false},
		{"int64 one", int64(1), true},
		{"bool false", false, false},
		{"bool true", true, true},
		{"string zero", "0", false},
		{"string one", "1", true},
		// Anything the schema cannot explain denies writes: wrongly refusing is
		// a clear error, wrongly allowing is the hole this closes.
		{"unknown type fails closed", struct{}{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readsOnly(tc.in); got != tc.want {
				t.Errorf("readsOnly(%#v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
