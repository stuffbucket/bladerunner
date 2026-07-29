package main

import "testing"

// `br web trust --system` installs the Incus certificate into the SYSTEM
// keychain, and 'br web trust' promises "Undo with 'br web untrust'". But
// --system was registered on the trust command only, while runWebUntrust read
// it: `br web untrust --system` died with "unknown flag", and the plain form
// always deleted from the LOGIN keychain — stranding a system-wide trusted
// certificate that no bladerunner command could remove.
//
// The flag is now on both commands, backed by the same variable, so untrust
// takes it and acts on the keychain the user named.
func TestWebUntrustTakesTheSystemFlag(t *testing.T) {
	saved := webTrustFlags
	t.Cleanup(func() {
		webTrustFlags = saved
		// The flagset holds its own copy of the parsed state.
		if f := webUntrustCmd.Flags().Lookup("system"); f != nil {
			_ = webUntrustCmd.Flags().Set("system", "false")
			f.Changed = false
		}
	})

	if webUntrustCmd.Flags().Lookup("system") == nil {
		t.Fatal("'br web untrust' does not declare --system, so it cannot undo 'br web trust --system'")
	}

	webTrustFlags.system = false
	if err := webUntrustCmd.ParseFlags([]string{"--system"}); err != nil {
		t.Fatalf("'br web untrust --system': %v", err)
	}
	// runWebUntrust reads webTrustFlags.system; parsing the flag on the untrust
	// command has to be what sets it.
	if !webTrustFlags.system {
		t.Fatal("--system parsed on 'br web untrust' did not reach the value runWebUntrust reads")
	}
}

// Both halves describe the same keychain, so the help of each has to say so.
func TestWebTrustAndUntrustDescribeTheSameKeychain(t *testing.T) {
	for _, name := range []string{"trust", "untrust"} {
		cmd := webTrustCmd
		if name == "untrust" {
			cmd = webUntrustCmd
		}
		f := cmd.Flags().Lookup("system")
		if f == nil {
			t.Errorf("'br web %s' does not declare --system", name)
			continue
		}
		if f.Usage == "" {
			t.Errorf("'br web %s --system' has no help text", name)
		}
	}
}
