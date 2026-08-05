package vm

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// The check must FAIL OPEN.
//
// It answers a question about code signing by shelling out to codesign, and
// every way that can go wrong — codesign absent, the binary unreadable, the
// call timing out — is a "cannot tell", not a "no". Refusing to start a VM on
// a question we could not answer would be worse than the hang this replaces,
// and VZ still makes the real decision and still explains itself.
func TestCheckSelfEntitlementFailsOpenOnACanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := CheckSelfEntitlement(ctx)
	if err != nil && !errors.Is(err, ErrMissingVirtualizationEntitlement) {
		t.Fatalf("returned an unexpected error kind: %v", err)
	}
	if runtime.GOOS != "darwin" && err != nil {
		t.Errorf("CheckSelfEntitlement = %v on %s, want nil: entitlements are a macOS concept", err, runtime.GOOS)
	}
}

// Off macOS the check is a no-op, so nothing needs a build tag to call it.
func TestCheckSelfEntitlementIsANoOpOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("meaningful only off macOS")
	}
	if err := CheckSelfEntitlement(context.Background()); err != nil {
		t.Errorf("CheckSelfEntitlement = %v, want nil on %s", err, runtime.GOOS)
	}
}

// The refusal must carry the guidance, not just the diagnosis.
//
// It is the same sentence VZ's own post-hoc error appends, so a user who hits
// this early and a user who hits it late are told the same thing.
func TestMissingEntitlementErrorNamesTheWayOut(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the actionable hint is macOS-specific")
	}
	msg := ErrMissingVirtualizationEntitlement.Error()
	for _, want := range []string{"make sign", "com.apple.security.virtualization"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}
}
