package admin

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tooltropolis/silo/internal/registry"
)

// settingsRow is one editable policy: the fleet default or a project override.
type settingsRow struct {
	Project string
	IsFleet bool
	Stored  registry.CacheSettings
	// TTLValue and friends render the form inputs. Empty means "inherit"; "0" is
	// an explicit zero, which is a real policy (never cache) rather than an
	// absence.
	TTLValue     string
	EntriesValue string
	BytesValue   string
	// EntryBytesValue is the per-entry write cap, not retention: it bounds what
	// may be written rather than what is kept.
	EntryBytesValue string
	UpdatedAt       string
	UpdatedBy       string
}

// handleSettings renders the policy editor and applies changes.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		s.fail(w, "settings", errors.New("no registry configured; cache policy cannot be stored"))
		return
	}
	if r.Method == http.MethodPost {
		s.applySettings(w, r)
		return
	}
	ctx := r.Context()

	all, err := s.settings.ListSettings(ctx)
	if err != nil {
		s.fail(w, "settings", err)
		return
	}

	// The fleet default always renders, even with no row stored, so there is
	// somewhere to set it from.
	rows := []settingsRow{newSettingsRow(registry.FleetKey, all[registry.FleetKey], true)}

	if s.registry != nil {
		recs, err := s.registry.List(ctx)
		if err != nil {
			s.fail(w, "settings", err)
			return
		}
		projectRows := make([]settingsRow, 0, len(recs))
		for _, rec := range recs {
			projectRows = append(projectRows, newSettingsRow(rec.ProjectID, all[rec.ProjectID], false))
		}
		sort.Slice(projectRows, func(i, j int) bool { return projectRows[i].Project < projectRows[j].Project })
		rows = append(rows, projectRows...)
	}

	s.render(w, "settings.html", map[string]any{
		"Active":   "settings",
		"Subtitle": "Cache retention, resolved per project then fleet then daemon flags",
		"Rows":     rows,
		"FleetKey": registry.FleetKey,
		"Flash":    r.URL.Query().Get("flash"),
		"FlashErr": r.URL.Query().Get("err"),
	})
}

func newSettingsRow(projectID string, s registry.CacheSettings, isFleet bool) settingsRow {
	row := settingsRow{
		Project:   projectID,
		IsFleet:   isFleet,
		Stored:    s,
		UpdatedAt: s.UpdatedAt,
		UpdatedBy: s.UpdatedBy,
	}
	// A nil field renders as an empty input, which reads back as "inherit". A
	// set zero renders as "0" and stays an explicit zero on save.
	if s.TTL != nil {
		row.TTLValue = s.TTL.String()
	}
	if s.MaxEntries != nil {
		row.EntriesValue = strconv.Itoa(*s.MaxEntries)
	}
	if s.MaxBytes != nil {
		row.BytesValue = strconv.FormatInt(*s.MaxBytes, 10)
	}
	if s.MaxEntryBytes != nil {
		row.EntryBytesValue = strconv.FormatInt(*s.MaxEntryBytes, 10)
	}
	return row
}

// applySettings writes one row's policy.
func (s *Server) applySettings(w http.ResponseWriter, r *http.Request) {
	projectID := r.FormValue("project")
	if projectID == "" {
		redirectErr(w, r, "/settings", "project required")
		return
	}

	// "Clear" removes the row entirely, restoring full inheritance — distinct
	// from saving a row whose fields are all blank, which is the same outcome
	// but keeps the audit metadata.
	if r.FormValue("action") == "clear" {
		if err := s.settings.DeleteSettings(r.Context(), projectID); err != nil {
			redirectErr(w, r, "/settings", err.Error())
			return
		}
		redirectFlash(w, r, "/settings", projectID+": overrides cleared")
		return
	}

	parsed, err := parseSettingsForm(r)
	if err != nil {
		redirectErr(w, r, "/settings", err.Error())
		return
	}
	parsed.UpdatedBy = actorFrom(r)

	if err := s.settings.PutSettings(r.Context(), projectID, parsed); err != nil {
		redirectErr(w, r, "/settings", err.Error())
		return
	}
	redirectFlash(w, r, "/settings", projectID+": policy saved — daemons pick it up on their next sweep")
}

// parseSettingsForm reads the three policy fields.
//
// An empty field is nil (inherit); a present field is a set value, including
// zero. Keeping those apart is the whole reason the columns are nullable, so
// the form must not collapse a blank into a 0.
func parseSettingsForm(r *http.Request) (registry.CacheSettings, error) {
	var out registry.CacheSettings

	if v := strings.TrimSpace(r.FormValue("ttl")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return out, fmt.Errorf("bad TTL %q: want a duration like 720h, 30m, or 0", v)
		}
		if d < 0 {
			return out, fmt.Errorf("bad TTL %q: must not be negative", v)
		}
		out.TTL = &d
	}
	if v := strings.TrimSpace(r.FormValue("max_entries")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return out, fmt.Errorf("bad max entries %q: want a whole number", v)
		}
		if n < 0 {
			return out, fmt.Errorf("bad max entries %q: must not be negative", v)
		}
		out.MaxEntries = &n
	}
	if v := strings.TrimSpace(r.FormValue("max_bytes")); v != "" {
		n, err := parseSize(v)
		if err != nil {
			return out, err
		}
		out.MaxBytes = &n
	}
	if v := strings.TrimSpace(r.FormValue("max_entry_bytes")); v != "" {
		n, err := parseSize(v)
		if err != nil {
			return out, fmt.Errorf("bad max entry size: %w", err)
		}
		out.MaxEntryBytes = &n
	}
	return out, nil
}

// parseSize accepts a plain byte count or a human suffix, so an operator can
// type "512MiB" rather than counting zeros.
func parseSize(v string) (int64, error) {
	s := strings.TrimSpace(v)
	multiplier := int64(1)
	for suffix, m := range map[string]int64{
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40,
		"KB": 1000, "MB": 1000 * 1000, "GB": 1000 * 1000 * 1000,
	} {
		if strings.HasSuffix(strings.ToUpper(s), strings.ToUpper(suffix)) {
			multiplier = m
			s = strings.TrimSpace(s[:len(s)-len(suffix)])
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad max bytes %q: want a byte count, optionally suffixed (e.g. 512MiB)", v)
	}
	if n < 0 {
		return 0, fmt.Errorf("bad max bytes %q: must not be negative", v)
	}
	return n * multiplier, nil
}

// actorFrom records who made a change, for the audit trail on the row.
//
// The console has no user model of its own — access is the socket or the token
// — so this is best-effort attribution, not authentication. It is labelled as
// such in the UI rather than presented as a verified identity.
func actorFrom(r *http.Request) string {
	if v := strings.TrimSpace(r.FormValue("actor")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-User"); v != "" {
		return v
	}
	return "console"
}
