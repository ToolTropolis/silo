// Package devstack supplies the throwaway credentials and helper paths that the
// local docker-compose stack uses, so routine local commands don't have to
// re-type them.
//
// These values are deliberately gated on the target being local. Baking a
// default token into a binary is how "dev-only-token" ends up authenticating
// against something real: the flag is convenient precisely because nobody reads
// it, so it stops being reviewed at exactly the moment it starts mattering.
// Applying the defaults only when every endpoint is loopback keeps the
// convenience local and makes a non-local run state its credentials explicitly.
package devstack

import (
	"net"
	"net/url"
	"strings"
)

// Dev-stack values, matching deploy/docker-compose.yaml and
// deploy/bootstrap-dev.sh. Throwaway by construction — the compose file has no
// TLS and the Vault instance is unsealed by a key committed to a volume.
const (
	VaultToken    = "dev-only-token"
	AdminKey      = "SILOADMIN"
	AdminSecret   = "SILOADMINSECRET"
	RuntimeKey    = "SILORUNTIME"
	RuntimeSecret = "SILORUNTIMESECRET"
)

// IsLocal reports whether every supplied endpoint resolves to loopback.
//
// An empty endpoint is ignored rather than treated as remote: callers pass a
// mix of flags, and an unset one carries no evidence either way. A value that
// cannot be parsed counts as non-local, so anything unrecognized fails toward
// requiring explicit credentials.
func IsLocal(endpoints ...string) bool {
	seen := false
	for _, ep := range endpoints {
		ep = strings.TrimSpace(ep)
		if ep == "" {
			continue
		}
		seen = true
		if !hostIsLoopback(ep) {
			return false
		}
	}
	return seen
}

// hostIsLoopback extracts the host from an address that may be a URL
// ("http://localhost:4001"), a host:port pair, or a bare hostname.
func hostIsLoopback(ep string) bool {
	host := ep
	if strings.Contains(ep, "://") {
		u, err := url.Parse(ep)
		if err != nil {
			return false
		}
		host = u.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]") // IPv6 literal

	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
