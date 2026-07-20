package vm

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestIsMissingEntitlementError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "full entitlement identifier in description",
			err: errors.New(`Error Domain=VZErrorDomain Code=2 ` +
				`Description="The process doesn't have the com.apple.security.virtualization entitlement." ` +
				`UserInfo={}`),
			want: true,
		},
		{
			name: "domain plus entitlement keyword only",
			err:  errors.New(`Error Domain=VZErrorDomain Code=2 Description="Missing entitlement." UserInfo={}`),
			want: true,
		},
		{
			name: "wrapped entitlement error",
			err: fmt.Errorf("create vm: %w",
				errors.New(`Error Domain=VZErrorDomain Code=2 Description="needs com.apple.security.virtualization" UserInfo={}`)),
			want: true,
		},
		{
			name: "unrelated vz configuration error",
			err:  errors.New(`Error Domain=VZErrorDomain Code=2 Description="The disk image is invalid." UserInfo={}`),
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("start vm: connection refused"),
			want: false,
		},
		{
			name: "entitlement keyword without vz domain",
			err:  errors.New("some unrelated entitlement problem"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMissingEntitlementError(tt.err); got != tt.want {
				t.Errorf("isMissingEntitlementError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAnnotateVZStartError(t *testing.T) {
	t.Run("wraps missing-entitlement error with hint", func(t *testing.T) {
		base := errors.New(`Error Domain=VZErrorDomain Code=2 Description="needs com.apple.security.virtualization" UserInfo={}`)
		got := annotateVZStartError(fmt.Errorf("create vm: %w", base))
		if got == nil {
			t.Fatal("annotateVZStartError returned nil")
		}
		if !errors.Is(got, base) {
			t.Errorf("annotateVZStartError broke the error chain; errors.Is(got, base) = false")
		}
		for _, want := range []string{"make sign", "Homebrew", virtualizationEntitlement, "create vm"} {
			if !strings.Contains(got.Error(), want) {
				t.Errorf("annotated error missing %q\n---\n%s", want, got.Error())
			}
		}
	})

	t.Run("passes through unrelated error unchanged", func(t *testing.T) {
		base := errors.New("start vm: connection refused")
		got := annotateVZStartError(base)
		if got == nil || got.Error() != base.Error() {
			t.Errorf("annotateVZStartError modified unrelated error: got %v, want %v", got, base)
		}
	})

	t.Run("nil passes through", func(t *testing.T) {
		if got := annotateVZStartError(nil); got != nil {
			t.Errorf("annotateVZStartError(nil) = %v, want nil", got)
		}
	})
}
