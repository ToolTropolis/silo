package admin

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"github.com/tooltropolis/silo/internal/registry"
)

// urlEscape makes a flash message safe to carry in a query string.
func urlEscape(s string) string { return url.QueryEscape(s) }

// cacheRow is one project's line in the cache view: what it is holding and what
// policy is being applied to it.
type cacheRow struct {
	Project string
	Stat    ProjectCacheStat
	// Policy is the resolved policy, pre-rendered for display. Each entry
	// carries where the value came from, so an operator can tell a project
	// override from an inherited fleet default without opening the settings
	// page. A policy you cannot attribute is a policy you cannot debug.
	Policy []policyField
	// StatsKnown is false when the daemon could not be reached for this project.
	// The view then shows "unknown" rather than zeros, because "nothing cached"
	// and "nobody checked" must not look the same.
	StatsKnown bool
}

// handleCache renders live cache state per project alongside the policy in
// force.
func (s *Server) handleCache(w http.ResponseWriter, r *http.Request) {
	if s.registry == nil {
		s.fail(w, "cache", errors.New("no registry configured"))
		return
	}
	ctx := r.Context()

	recs, err := s.registry.List(ctx)
	if err != nil {
		s.fail(w, "cache", err)
		return
	}

	// Stats are best-effort: a daemon that is down must not blank the whole
	// page, since the policy half is still readable and actionable.
	stats := map[string]ProjectCacheStat{}
	var daemonErr error
	if s.daemon != nil {
		list, err := s.daemon.CacheStats(ctx)
		if err != nil {
			daemonErr = err
		}
		for _, st := range list {
			stats[st.Project] = st
		}
	} else {
		daemonErr = errors.New("no daemon configured (--daemon); cache sizes and actions are unavailable")
	}

	all := map[string]registry.CacheSettings{}
	if s.settings != nil {
		if all, err = s.settings.ListSettings(ctx); err != nil {
			s.fail(w, "cache", err)
			return
		}
	}
	fleet := all[registry.FleetKey]

	rows := make([]cacheRow, 0, len(recs))
	var totalPending, totalEntries int
	var reclaimable int64
	for _, rec := range recs {
		st, known := stats[rec.ProjectID]
		rows = append(rows, cacheRow{
			Project:    rec.ProjectID,
			Stat:       st,
			Policy:     resolveWithSource(all[rec.ProjectID], fleet),
			StatsKnown: known && st.StatsError == "",
		})
		reclaimable += st.Reclaimable()
		totalPending += st.Pending
		totalEntries += st.Entries
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Project < rows[j].Project })

	s.render(w, "cache.html", map[string]any{
		"Active":           "cache",
		"Subtitle":         "Local cache state and the retention policy in force",
		"Rows":             rows,
		"DaemonErr":        errString(daemonErr),
		"TotalReclaimable": reclaimable,
		"TotalPending":     totalPending,
		"TotalEntries":     totalEntries,
		"CanAct":           s.daemon != nil,
		"Flash":            r.URL.Query().Get("flash"),
		"FlashErr":         r.URL.Query().Get("err"),
	})
}

// policyField is one resolved setting, ready to render.
type policyField struct {
	Label string
	Value string
	// Source is "project", "fleet", or "daemon" — where the value came from.
	Source string
}

// resolveWithSource resolves a policy and records where each field came from.
//
// The daemon's flags are deliberately not represented as a distinct source: the
// console cannot see them (they are per-host and may differ across the fleet),
// so naming a specific flag value would be a guess. A field set at neither
// level is attributed to the daemon, which is exactly where it is decided.
func resolveWithSource(project, fleet registry.CacheSettings) []policyField {
	effective := registry.Resolve(project, fleet)

	source := func(p, f bool) string {
		switch {
		case p:
			return "project"
		case f:
			return "fleet"
		default:
			return "daemon"
		}
	}

	ttl := policyField{Label: "TTL", Value: "daemon default",
		Source: source(project.TTL != nil, fleet.TTL != nil)}
	if effective.TTL != nil {
		ttl.Value = humanDuration(*effective.TTL)
	}

	entries := policyField{Label: "Max entries", Value: "daemon default",
		Source: source(project.MaxEntries != nil, fleet.MaxEntries != nil)}
	if effective.MaxEntries != nil {
		entries.Value = strconv.Itoa(*effective.MaxEntries)
	}

	maxBytes := policyField{Label: "Max bytes", Value: "daemon default",
		Source: source(project.MaxBytes != nil, fleet.MaxBytes != nil)}
	if effective.MaxBytes != nil {
		maxBytes.Value = humanBytes(*effective.MaxBytes)
	}

	return []policyField{ttl, entries, maxBytes}
}

// handleCacheAction runs purge or compact against one project.
func (s *Server) handleCacheAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.daemon == nil {
		redirectErr(w, r, "/", "no daemon configured")
		return
	}
	projectID := r.FormValue("project")
	if projectID == "" {
		redirectErr(w, r, "/", "project required")
		return
	}

	switch r.FormValue("action") {
	case "compact":
		res, err := s.daemon.CompactCache(r.Context(), projectID)
		if err != nil {
			redirectErr(w, r, "/", err.Error())
			return
		}
		if !res.Compacted {
			// A skip is the safety gate working, so report it as information
			// rather than as a failure the operator needs to chase.
			redirectFlash(w, r, "/", projectID+": compaction skipped — "+res.SkipReason)
			return
		}
		redirectFlash(w, r, "/", projectID+": reclaimed "+humanBytes(res.Reclaimed)+
			" ("+humanBytes(res.BytesBefore)+" → "+humanBytes(res.BytesAfter)+")")

	case "purge":
		res, err := s.daemon.PurgeCache(r.Context(), projectID)
		if err != nil {
			redirectErr(w, r, "/", err.Error())
			return
		}
		if !res.Purged {
			redirectErr(w, r, "/", projectID+": purge refused — "+res.Reason)
			return
		}
		redirectFlash(w, r, "/", projectID+": local cache purged")

	default:
		redirectErr(w, r, "/", "unknown action")
	}
}

// redirectFlash and redirectErr POST-redirect-GET so a refresh cannot replay a
// destructive action.
func redirectFlash(w http.ResponseWriter, r *http.Request, path, msg string) {
	http.Redirect(w, r, path+"?flash="+urlEscape(msg), http.StatusSeeOther)
}

func redirectErr(w http.ResponseWriter, r *http.Request, path, msg string) {
	http.Redirect(w, r, path+"?err="+urlEscape(msg), http.StatusSeeOther)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
