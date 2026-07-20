package incus

import (
	"strings"
	"testing"

	"github.com/lxc/incus/v6/shared/api"
)

// TestCheckAuthorized pins the readiness gate: the Incus API answers GetServer
// even before it has accepted our client cert (Auth=="untrusted"), so readiness
// must distinguish a trusted response from a merely-reachable one. A regression
// here would let a half-started VM whose cert never landed in the trust store be
// reported as ready — the bug this gate fixes.
func TestCheckAuthorized(t *testing.T) {
	tests := []struct {
		name    string
		server  *api.Server
		wantErr bool
	}{
		{
			name:    "trusted is ready",
			server:  &api.Server{ServerUntrusted: api.ServerUntrusted{Auth: authTrusted}},
			wantErr: false,
		},
		{
			name:    "untrusted is not ready",
			server:  &api.Server{ServerUntrusted: api.ServerUntrusted{Auth: "untrusted"}},
			wantErr: true,
		},
		{
			name:    "empty auth is not ready",
			server:  &api.Server{ServerUntrusted: api.ServerUntrusted{Auth: ""}},
			wantErr: true,
		},
		{
			name:    "nil server is not ready",
			server:  nil,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAuthorized(tc.server)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("checkAuthorized(%+v) = nil, want error", tc.server)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkAuthorized(%+v) = %v, want nil", tc.server, err)
			}
		})
	}
}

// TestCheckAuthorizedErrorSurfacesAuth ensures the untrusted error names the
// observed auth state so the readiness loop's logs point at trust drift rather
// than a generic "not ready".
func TestCheckAuthorizedErrorSurfacesAuth(t *testing.T) {
	err := checkAuthorized(&api.Server{ServerUntrusted: api.ServerUntrusted{Auth: "untrusted"}})
	if err == nil {
		t.Fatal("expected error for untrusted server")
	}
	if !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("error %q does not surface the auth state", err)
	}
}
