//go:build darwin

package main

import (
	"slices"
	"testing"
)

// The whole point of `br web untrust` is to remove what `br web trust` added,
// so the two must name the SAME keychain for the same --system value. They are
// asserted here on the argument vectors alone: nothing in this test runs
// `security`, so no real keychain is read or written.
func TestTrustAndUntrustNameTheSameKeychain(t *testing.T) {
	for _, system := range []bool{false, true} {
		installName, installArgs := trustCertCommand("/tmp/cert.pem", system)
		removeName, removeArgs := untrustCertCommand(system)

		if installName != removeName {
			t.Errorf("system=%v: trust runs %q but untrust runs %q", system, installName, removeName)
		}
		installKeychain := keychainArg(t, installArgs)
		removeKeychain := keychainArg(t, removeArgs)
		if installKeychain != removeKeychain {
			t.Errorf("system=%v: trust installs into %q but untrust deletes from %q",
				system, installKeychain, removeKeychain)
		}
	}

	// And the two values are genuinely different keychains, so the flag is not
	// decorative: --system escalates and targets the system store.
	_, loginArgs := untrustCertCommand(false)
	_, systemArgs := untrustCertCommand(true)
	if keychainArg(t, loginArgs) == keychainArg(t, systemArgs) {
		t.Error("--system deletes from the same keychain as the login form")
	}
	if name, _ := untrustCertCommand(true); name != sudoCmd {
		t.Errorf("untrust --system runs %q, want %q (the system keychain needs it)", name, sudoCmd)
	}
	if name, _ := untrustCertCommand(false); name != securityCmd {
		t.Errorf("untrust runs %q, want %q", name, securityCmd)
	}
}

// keychainArg returns the keychain path in a `security` argument vector: the
// last argument for delete-certificate, and the value after -k for
// add-trusted-cert.
func keychainArg(t *testing.T, args []string) string {
	t.Helper()
	if i := slices.Index(args, "-k"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	if len(args) == 0 {
		t.Fatal("empty security argument vector")
	}
	return args[len(args)-1]
}
