package main

import (
	"strings"
	"testing"
)

// The listener is the authorization boundary, so the rules about what may be
// bound without a token are a security invariant, not a convenience check.
func TestCheckListenSafety(t *testing.T) {
	tests := []struct {
		name        string
		listen      string
		token       string
		allowRemote bool
		wantErr     string // substring; empty means it must be permitted
	}{
		{
			name:   "a unix socket needs no token: the filesystem is the boundary",
			listen: "./data/silo-admin.sock",
		},
		{
			name:   "an absolute socket path is fine too",
			listen: "/var/run/silo/admin.sock",
		},
		{
			// Without this, a surface that can delete a project's bucket would
			// be reachable by anyone who can route to the port.
			name:    "TCP without a token is refused",
			listen:  "127.0.0.1:8700",
			wantErr: "--admin-token is required",
		},
		{
			name:   "loopback TCP with a token is permitted",
			listen: "127.0.0.1:8700",
			token:  "secret",
		},
		{
			name:   "IPv6 loopback with a token is permitted",
			listen: "[::1]:8700",
			token:  "secret",
		},
		{
			// A token alone is not enough to justify publishing teardown to the
			// network; that needs a second, explicit opt-in.
			name:    "binding every interface needs --allow-remote",
			listen:  "0.0.0.0:8700",
			token:   "secret",
			wantErr: "--allow-remote",
		},
		{
			name:    "an empty host binds everything too",
			listen:  ":8700",
			token:   "secret",
			wantErr: "--allow-remote",
		},
		{
			name:    "IPv6 wildcard needs --allow-remote",
			listen:  "[::]:8700",
			token:   "secret",
			wantErr: "--allow-remote",
		},
		{
			name:    "a routable address needs --allow-remote",
			listen:  "10.0.0.5:8700",
			token:   "secret",
			wantErr: "--allow-remote",
		},
		{
			name:        "explicit opt-in permits a wildcard bind",
			listen:      "0.0.0.0:8700",
			token:       "secret",
			allowRemote: true,
		},
		{
			name:        "explicit opt-in permits a routable address",
			listen:      "10.0.0.5:8700",
			token:       "secret",
			allowRemote: true,
		},
		{
			// --allow-remote must not substitute for a token.
			name:        "allow-remote alone still requires a token",
			listen:      "0.0.0.0:8700",
			allowRemote: true,
			wantErr:     "--admin-token is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkListenSafety(tc.listen, tc.token, tc.allowRemote)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("checkListenSafety(%q) = %v, want it permitted", tc.listen, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkListenSafety(%q, token=%q, allowRemote=%v) was permitted, want it refused",
					tc.listen, tc.token, tc.allowRemote)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// isSocketPath must agree with daemon.Listen's own rule, or the two would
// disagree about which kind of listener is being created — and the safety check
// would guard the wrong thing.
func TestIsSocketPath(t *testing.T) {
	sockets := []string{"./data/admin.sock", "/var/run/silo.sock", "admin.sock", "data/x"}
	for _, s := range sockets {
		if !isSocketPath(s) {
			t.Errorf("isSocketPath(%q) = false, want true", s)
		}
	}
	tcp := []string{"127.0.0.1:8700", ":8700", "0.0.0.0:8700", "[::1]:8700"}
	for _, s := range tcp {
		if isSocketPath(s) {
			t.Errorf("isSocketPath(%q) = true, want false", s)
		}
	}
}
