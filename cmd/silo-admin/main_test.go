package main

import (
	"os"
	"path/filepath"
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

// A bare command name in .mcp.json forces the agent to have silo-mcp on its
// PATH, which an agent launched from a GUI or another shell usually does not.
func TestResolveMCPBinary_FindsItOnPATH(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "silo-mcp")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", dir)

	got := resolveMCPBinary()
	if !filepath.IsAbs(got) {
		t.Errorf("resolveMCPBinary() = %q, want an absolute path", got)
	}
	if got != bin {
		t.Errorf("resolveMCPBinary() = %q, want %q", got, bin)
	}
}

// Not found: still emit a usable file rather than failing, and warn.
func TestResolveMCPBinary_FallsBackToBareName(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	if got := resolveMCPBinary(); got != "silo-mcp" {
		t.Errorf("resolveMCPBinary() = %q, want the bare name as a fallback", got)
	}
}

// The console needs the S3 admin credential to onboard or tear down. Against
// the dev stack those are the values bootstrap-dev.sh just provisioned, so
// requiring them to be re-typed only produces a console that silently cannot
// provision.
func TestApplyDevDefaults_FillsLocalCredentials(t *testing.T) {
	var vaultToken, accessKey, secretKey string
	applyDevDefaults("http://localhost:8333", "http://localhost:4001",
		"http://localhost:8201", "localhost:8888",
		&vaultToken, &accessKey, &secretKey)

	if vaultToken == "" || accessKey == "" || secretKey == "" {
		t.Fatalf("loopback endpoints should be filled, got token=%q access=%q secret=%q",
			vaultToken, accessKey, secretKey)
	}
}

// The gate is the security-relevant half: a console pointed at a real cluster
// must not fall back to a credential compiled into the binary.
func TestApplyDevDefaults_RefusesNonLocalEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		backend, rqlite, vault, filer string
	}{
		{"remote backend", "http://s3.example.com", "http://localhost:4001", "http://localhost:8201", "localhost:8888"},
		{"remote rqlite", "http://localhost:8333", "http://rqlite.example.com", "http://localhost:8201", "localhost:8888"},
		{"remote vault", "http://localhost:8333", "http://localhost:4001", "https://vault.example.com", "localhost:8888"},
		{"remote filer", "http://localhost:8333", "http://localhost:4001", "http://localhost:8201", "filer.example.com:8888"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var vaultToken, accessKey, secretKey string
			applyDevDefaults(tc.backend, tc.rqlite, tc.vault, tc.filer,
				&vaultToken, &accessKey, &secretKey)

			if vaultToken != "" || accessKey != "" || secretKey != "" {
				t.Errorf("a non-local endpoint must not receive baked-in credentials, "+
					"got token=%q access=%q secret=%q", vaultToken, accessKey, secretKey)
			}
		})
	}
}

// An explicit credential always wins, so an operator can point a local console
// at a non-default identity without the defaults overwriting it.
func TestApplyDevDefaults_DoesNotOverrideExplicitValues(t *testing.T) {
	vaultToken, accessKey, secretKey := "mine", "MYKEY", "MYSECRET"
	applyDevDefaults("http://localhost:8333", "http://localhost:4001",
		"http://localhost:8201", "localhost:8888",
		&vaultToken, &accessKey, &secretKey)

	if vaultToken != "mine" || accessKey != "MYKEY" || secretKey != "MYSECRET" {
		t.Errorf("explicit values were overwritten: token=%q access=%q secret=%q",
			vaultToken, accessKey, secretKey)
	}
}
