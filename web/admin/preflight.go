package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tooltropolis/silo/internal/project"
	"github.com/tooltropolis/silo/internal/registry"
)

// CheckStatus is the outcome of one preflight check.
type CheckStatus string

const (
	CheckPass CheckStatus = "pass"
	CheckWarn CheckStatus = "warn" // proceed, but the operator should know
	CheckFail CheckStatus = "fail" // onboarding would fail; block
)

// PreflightCheck is one verifiable precondition for onboarding.
//
// The point of running these before provisioning: onboarding creates four
// layers with a compensating-action rollback, and a failure at layer four
// unwinds the other three. A rollback that works is still worse than never
// having started, because the operator has to read a two-part error to work out
// that nothing survived. These checks turn most of those failures into a
// message shown before anything is created.
type PreflightCheck struct {
	Name   string
	Status CheckStatus
	Detail string
	// Fix is what the operator should do about a warn or fail.
	Fix string
}

// PreflightReport is the full set of checks plus whether onboarding may start.
type PreflightReport struct {
	ProjectID string
	Checks    []PreflightCheck
	// Blocked is true when any check failed. A warn never blocks.
	Blocked bool
}

// Blockers returns just the failing checks, for a compact summary.
func (r PreflightReport) Blockers() []PreflightCheck {
	var out []PreflightCheck
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			out = append(out, c)
		}
	}
	return out
}

// Preflighter runs the checks. Each dependency is optional; a nil one yields a
// warn ("not configured, cannot verify") rather than a false pass, because
// "unchecked" and "healthy" must not look the same on a page whose whole job is
// to say whether provisioning will work.
type Preflighter struct {
	Registry Registry
	Daemon   DaemonAdmin
	// Backend probes the durable backend. Optional.
	Backend BackendProber
	// Creds probes the credential issuer, which is the layer most likely to
	// fail — it shells out to `weed`, which may be absent or unable to reach a
	// containerized cluster.
	Creds CredentialProber
}

// BackendProber reports whether the durable backend is reachable.
type BackendProber interface {
	Probe(ctx context.Context) error
}

// CredentialProber reports whether per-project credentials can be issued.
type CredentialProber interface {
	Probe(ctx context.Context) error
}

// Run executes every check for a candidate project ID.
func (p *Preflighter) Run(ctx context.Context, projectID string) PreflightReport {
	report := PreflightReport{ProjectID: projectID}
	add := func(c PreflightCheck) {
		if c.Status == CheckFail {
			report.Blocked = true
		}
		report.Checks = append(report.Checks, c)
	}

	add(p.checkID(projectID))
	add(p.checkAvailable(ctx, projectID))
	add(p.checkRegistry(ctx))
	add(p.checkCredentials(ctx))
	add(p.checkBackend(ctx))
	add(p.checkCacheCollision(ctx, projectID))

	return report
}

// checkID validates the ID before anything else looks at it. This is the one
// check that is genuinely permanent: the ID becomes a bucket name and a cache
// filename, and neither can be renamed afterwards.
func (p *Preflighter) checkID(projectID string) PreflightCheck {
	c := PreflightCheck{Name: "Project ID is valid"}
	if err := project.ValidateID(projectID); err != nil {
		c.Status = CheckFail
		c.Detail = err.Error()
		c.Fix = "Use 3–56 characters: lowercase letters, digits, and hyphens only."
		return c
	}
	c.Status = CheckPass
	c.Detail = fmt.Sprintf("Bucket will be %q; cache file %q.", "silo-"+projectID, projectID+".bbolt")
	return c
}

// checkAvailable makes sure the ID is not already taken. Onboarding would fail
// at the registry step anyway, but reporting it here costs nothing and the
// message is far clearer.
func (p *Preflighter) checkAvailable(ctx context.Context, projectID string) PreflightCheck {
	c := PreflightCheck{Name: "Project ID is available"}
	if p.Registry == nil {
		c.Status = CheckWarn
		c.Detail = "No registry configured, so this cannot be verified."
		return c
	}
	_, err := p.Registry.Get(ctx, projectID)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		c.Status = CheckPass
		c.Detail = "Not registered."
	case err != nil:
		c.Status = CheckWarn
		c.Detail = "Could not check: " + err.Error()
		c.Fix = "Verify the registry is reachable."
	default:
		c.Status = CheckFail
		c.Detail = fmt.Sprintf("%q is already registered.", projectID)
		c.Fix = "Choose a different ID, or tear the existing project down first."
	}
	return c
}

func (p *Preflighter) checkRegistry(ctx context.Context) PreflightCheck {
	c := PreflightCheck{Name: "Registry is reachable"}
	if p.Registry == nil {
		c.Status = CheckFail
		c.Detail = "No registry configured."
		c.Fix = "Start silo-admin with --rqlite pointing at the cluster."
		return c
	}
	if _, err := p.Registry.List(ctx); err != nil {
		c.Status = CheckFail
		c.Detail = err.Error()
		c.Fix = "Check the rqlite cluster is up and reachable."
		return c
	}
	c.Status = CheckPass
	c.Detail = "Connected."
	return c
}

// checkCredentials probes the layer most likely to fail. Issuing a scoped
// credential shells out to `weed`, which may be missing from PATH or — against
// a containerized cluster — unable to reach the addresses SeaweedFS advertises,
// in which case it hangs rather than failing.
func (p *Preflighter) checkCredentials(ctx context.Context) PreflightCheck {
	c := PreflightCheck{Name: "Credential issuer is working"}
	if p.Creds == nil {
		c.Status = CheckWarn
		c.Detail = "Not configured, so this cannot be verified."
		c.Fix = "Onboarding will fail at the credential step if `weed` is unreachable."
		return c
	}
	// Bounded: the failure mode here is a hang, not an error, so a probe
	// without a deadline would reproduce exactly the problem it is checking for.
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := p.Creds.Probe(probeCtx); err != nil {
		c.Status = CheckFail
		c.Detail = err.Error()
		c.Fix = "Install the SeaweedFS CLI (`brew install seaweedfs`). Against the Docker " +
			"dev stack, run silo-admin where `weed` can reach the addresses SeaweedFS " +
			"advertises — a host-native `weed` cannot route to a container IP and will hang."
		return c
	}
	c.Status = CheckPass
	c.Detail = "`weed` responded."
	return c
}

func (p *Preflighter) checkBackend(ctx context.Context) PreflightCheck {
	c := PreflightCheck{Name: "Storage backend is reachable"}
	if p.Backend == nil {
		c.Status = CheckWarn
		c.Detail = "Not configured, so this cannot be verified."
		return c
	}
	if err := p.Backend.Probe(ctx); err != nil {
		c.Status = CheckFail
		c.Detail = err.Error()
		c.Fix = "Check the S3 endpoint and that the admin credentials are correct."
		return c
	}
	c.Status = CheckPass
	c.Detail = "Connected."
	return c
}

// checkCacheCollision warns when a daemon still holds a cache file for this ID.
//
// Not a blocker: the generation stamp means a new project cannot read an old
// one's cached memory — the file is wiped on first bind. But an operator
// reusing an ID should know the file is there, because it means a previous
// project of that name was not fully cleaned up.
func (p *Preflighter) checkCacheCollision(ctx context.Context, projectID string) PreflightCheck {
	c := PreflightCheck{Name: "No leftover local cache"}
	if p.Daemon == nil {
		c.Status = CheckWarn
		c.Detail = "No daemon configured, so local caches cannot be checked."
		return c
	}
	stats, err := p.Daemon.CacheStats(ctx)
	if err != nil {
		c.Status = CheckWarn
		c.Detail = "Could not reach the daemon: " + err.Error()
		return c
	}
	for _, s := range stats {
		if s.Project != projectID {
			continue
		}
		c.Status = CheckWarn
		c.Detail = fmt.Sprintf("A daemon still holds a cache file for %q (%d entries, %s).",
			projectID, s.Entries, humanBytes(s.FileBytes))
		c.Fix = "Safe to proceed: the generation stamp means the new project cannot read " +
			"the old cached memory, which is discarded on first use. It does suggest a " +
			"previous project of this name was not fully torn down."
		return c
	}
	c.Status = CheckPass
	c.Detail = "No daemon holds a cache for this ID."
	return c
}
