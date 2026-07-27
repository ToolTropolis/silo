package devstack

import "testing"

func TestIsLocal(t *testing.T) {
	cases := []struct {
		name      string
		endpoints []string
		want      bool
	}{
		{"localhost url", []string{"http://localhost:4001"}, true},
		{"loopback ip", []string{"http://127.0.0.1:8333"}, true},
		{"ipv6 loopback", []string{"http://[::1]:8200"}, true},
		{"host:port", []string{"localhost:8888"}, true},
		{"bare host", []string{"127.0.0.1"}, true},
		{"all local", []string{"http://localhost:4001", "http://127.0.0.1:8201"}, true},

		{"remote host", []string{"https://vault.internal:8200"}, false},
		{"remote ip", []string{"http://10.0.0.5:8333"}, false},
		// One remote endpoint is enough: partially-local means the command is
		// talking to something real, so nothing may be defaulted.
		{"mixed", []string{"http://localhost:4001", "https://vault.prod:8200"}, false},
		// Unparseable fails toward requiring explicit credentials.
		{"garbage", []string{"://:::"}, false},

		// No evidence either way must not be read as local.
		{"empty", []string{""}, false},
		{"none", nil, false},
		{"empty ignored among local", []string{"", "http://localhost:4001"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLocal(tc.endpoints...); got != tc.want {
				t.Errorf("IsLocal(%q) = %v, want %v", tc.endpoints, got, tc.want)
			}
		})
	}
}

// A remote endpoint must never receive the dev token, whatever else is local.
func TestIsLocal_RemoteNeverDefaults(t *testing.T) {
	remotes := []string{
		"https://vault.example.com",
		"http://192.168.1.10:8200",
		"http://silo-prod.internal:4001",
	}
	for _, r := range remotes {
		if IsLocal("http://localhost:4001", "http://localhost:8333", r) {
			t.Errorf("%q was treated as local; dev credentials would be applied to it", r)
		}
	}
}
