package admin

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// healthTTL bounds how stale the header indicator can be. Short enough that a
// daemon dying is noticed within a page load or two, long enough that clicking
// around does not probe on every request.
const healthTTL = 10 * time.Second

// DaemonHealth is what the header shows.
type DaemonHealth struct {
	// Configured is false when no --daemon was given at all, which is a
	// different thing from a daemon that is down.
	Configured bool
	Up         bool
	Detail     string
}

// health probes the daemon, cached so every page render does not hit it.
//
// A cheap CacheStats call doubles as the liveness check: it is the same request
// the Cache page makes, so a working header implies a working page.
func (s *Server) health() DaemonHealth {
	if s.daemon == nil {
		return DaemonHealth{Detail: "not configured"}
	}

	s.healthMu.Lock()
	if time.Since(s.healthAt) < healthTTL {
		cached := s.healthCache
		s.healthMu.Unlock()
		return cached
	}
	s.healthMu.Unlock()

	// Bounded: an unreachable daemon must not hang every page in the console.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	h := DaemonHealth{Configured: true}
	if stats, err := s.daemon.CacheStats(ctx); err != nil {
		h.Detail = err.Error()
	} else {
		h.Up = true
		h.Detail = pluralProjects(len(stats))
	}

	s.healthMu.Lock()
	s.healthCache, s.healthAt = h, time.Now()
	s.healthMu.Unlock()
	return h
}

func pluralProjects(n int) string {
	if n == 1 {
		return "1 project"
	}
	return strconv.Itoa(n) + " projects"
}

// healthState is embedded in Server.
type healthState struct {
	healthMu    sync.Mutex
	healthCache DaemonHealth
	healthAt    time.Time
}
