package admin

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tooltropolis/silo/internal/registry"
)

// The distinction the whole nullable schema exists for, enforced at the form
// boundary: a blank field must stay nil (inherit), and a typed zero must become
// a set zero (a real policy — never cache).
func TestSettingsForm_BlankIsInheritZeroIsExplicit(t *testing.T) {
	tests := []struct {
		name        string
		form        url.Values
		wantTTL     *time.Duration
		wantEntries *int
		wantBytes   *int64
	}{
		{
			name:        "all blank means inherit everything",
			form:        url.Values{"ttl": {""}, "max_entries": {""}, "max_bytes": {""}},
			wantTTL:     nil,
			wantEntries: nil,
			wantBytes:   nil,
		},
		{
			name:        "omitted fields are inherit too",
			form:        url.Values{},
			wantTTL:     nil,
			wantEntries: nil,
			wantBytes:   nil,
		},
		{
			name:        "a typed zero is an explicit policy, not an absence",
			form:        url.Values{"ttl": {"0"}, "max_entries": {"0"}, "max_bytes": {"0"}},
			wantTTL:     ttl(0),
			wantEntries: entries(0),
			wantBytes:   bytes64(0),
		},
		{
			name:        "whitespace is treated as blank",
			form:        url.Values{"ttl": {"   "}, "max_entries": {"\t"}},
			wantTTL:     nil,
			wantEntries: nil,
		},
		{
			name:        "durations parse",
			form:        url.Values{"ttl": {"720h"}},
			wantTTL:     ttl(720 * time.Hour),
			wantEntries: nil,
		},
		{
			name:      "byte suffixes parse",
			form:      url.Values{"max_bytes": {"512MiB"}},
			wantBytes: bytes64(512 << 20),
		},
		{
			name:      "a plain byte count parses",
			form:      url.Values{"max_bytes": {"1048576"}},
			wantBytes: bytes64(1 << 20),
		},
		{
			name:        "one field set leaves the others inheriting",
			form:        url.Values{"max_entries": {"250"}},
			wantTTL:     nil,
			wantEntries: entries(250),
			wantBytes:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := formRequest(tc.form)
			got, err := parseSettingsForm(r)
			if err != nil {
				t.Fatalf("parseSettingsForm: %v", err)
			}
			if !eqDurP(got.TTL, tc.wantTTL) {
				t.Errorf("TTL = %v, want %v", showDurP(got.TTL), showDurP(tc.wantTTL))
			}
			if !eqIntP(got.MaxEntries, tc.wantEntries) {
				t.Errorf("MaxEntries = %v, want %v", showIntP(got.MaxEntries), showIntP(tc.wantEntries))
			}
			if !eqI64P(got.MaxBytes, tc.wantBytes) {
				t.Errorf("MaxBytes = %v, want %v", showI64P(got.MaxBytes), showI64P(tc.wantBytes))
			}
		})
	}
}

func TestSettingsForm_RejectsBadInput(t *testing.T) {
	bad := []url.Values{
		{"ttl": {"soon"}},
		{"ttl": {"-1h"}},
		{"max_entries": {"lots"}},
		{"max_entries": {"-5"}},
		{"max_entries": {"1.5"}},
		{"max_bytes": {"-1"}},
		{"max_bytes": {"512 gigabytes"}},
	}
	for _, form := range bad {
		if _, err := parseSettingsForm(formRequest(form)); err == nil {
			t.Errorf("parseSettingsForm(%v) should be rejected", form)
		}
	}
}

// A save must store exactly what was typed, including the nil/zero distinction
// the form preserved.
func TestSettings_SaveStoresWhatWasTyped(t *testing.T) {
	fs := newFakeSettings(nil)
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: fs})

	resp := postForm(t, ts, "/settings", url.Values{
		"project":     {"proj-11"},
		"max_entries": {"250"},
		"actor":       {"nav"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}

	put, ok := fs.lastPut()
	if !ok {
		t.Fatal("nothing was stored")
	}
	if put.project != "proj-11" {
		t.Errorf("stored for %q, want proj-11", put.project)
	}
	if put.settings.MaxEntries == nil || *put.settings.MaxEntries != 250 {
		t.Errorf("MaxEntries = %v, want 250", showIntP(put.settings.MaxEntries))
	}
	// The untouched fields must be stored as NULL, not as zero, or the project
	// would be pinned to "never cache" by a form it never filled in.
	if put.settings.TTL != nil {
		t.Errorf("TTL = %v, want nil so it keeps inheriting", showDurP(put.settings.TTL))
	}
	if put.settings.UpdatedBy != "nav" {
		t.Errorf("UpdatedBy = %q, want nav", put.settings.UpdatedBy)
	}
}

// Clear removes the row entirely, restoring full inheritance.
func TestSettings_ClearDeletesTheRow(t *testing.T) {
	fs := newFakeSettings(map[string]registry.CacheSettings{
		"proj-11": {MaxEntries: entries(5)},
	})
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: fs})

	postForm(t, ts, "/settings", url.Values{
		"project": {"proj-11"},
		"action":  {"clear"},
	})

	if len(fs.deleted) != 1 || fs.deleted[0] != "proj-11" {
		t.Errorf("deleted = %v, want [proj-11]", fs.deleted)
	}
}

func TestSettings_RequiresAProject(t *testing.T) {
	fs := newFakeSettings(nil)
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: fs})

	resp := postForm(t, ts, "/settings", url.Values{"max_entries": {"5"}})
	_, errMsg := flashOf(t, resp)
	if errMsg == "" {
		t.Error("a save with no project must report an error")
	}
	if len(fs.puts) != 0 {
		t.Errorf("stored %v despite no project being named", fs.puts)
	}
}

// A bad value must not be silently dropped or coerced — it must be reported and
// nothing written.
func TestSettings_BadValueIsReportedAndNotStored(t *testing.T) {
	fs := newFakeSettings(nil)
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: fs})

	resp := postForm(t, ts, "/settings", url.Values{
		"project": {"proj-11"},
		"ttl":     {"whenever"},
	})
	_, errMsg := flashOf(t, resp)
	if !strings.Contains(errMsg, "TTL") {
		t.Errorf("error = %q, want it to name the bad field", errMsg)
	}
	if len(fs.puts) != 0 {
		t.Errorf("stored %v despite invalid input", fs.puts)
	}
}

// The fleet default row must always render, even with nothing stored, or there
// is nowhere to set it from.
func TestSettings_FleetRowAlwaysRenders(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: newFakeSettings(nil)})

	body := getBody(t, ts, "/settings")
	if !strings.Contains(body, registry.FleetKey) {
		t.Error("the fleet default row must render even when unset")
	}
	if !strings.Contains(body, "proj-11") {
		t.Error("registered projects should each get a row")
	}
}

// Without a registry there is nowhere to store policy; say so rather than
// rendering an editor whose saves go nowhere.
func TestSettings_NoStoreReportsUnavailable(t *testing.T) {
	ts := newFixture(t, Config{Registry: activeProject("proj-11")})

	resp, err := noRedirectClient().Get(ts.URL + "/settings")
	if err != nil {
		t.Fatalf("GET /settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 when no settings store is configured", resp.StatusCode)
	}
}

func TestSettings_StoreErrorIsSurfaced(t *testing.T) {
	fs := newFakeSettings(nil)
	fs.err = errors.New("rqlite unreachable")
	ts := newFixture(t, Config{Registry: activeProject("proj-11"), Settings: fs})

	resp, err := noRedirectClient().Get(ts.URL + "/settings")
	if err != nil {
		t.Fatalf("GET /settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want the registry failure surfaced", resp.StatusCode)
	}
}

// --- helpers ---------------------------------------------------------------

func formRequest(form url.Values) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func getBody(t *testing.T, ts *httptest.Server, path string) string {
	t.Helper()
	resp, err := noRedirectClient().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func eqDurP(a, b *time.Duration) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqIntP(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func eqI64P(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func showDurP(d *time.Duration) string {
	if d == nil {
		return "inherit"
	}
	return d.String()
}

func showIntP(n *int) string {
	if n == nil {
		return "inherit"
	}
	return strconv.Itoa(*n)
}

func showI64P(n *int64) string {
	if n == nil {
		return "inherit"
	}
	return strconv.FormatInt(*n, 10)
}
