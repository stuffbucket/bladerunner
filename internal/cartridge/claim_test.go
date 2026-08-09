// The export test for the boot-claim probe. Busy and Claim are what a front end
// outside this package asks before a destructive step (`br boot` refusing a
// cartridge that is already running), so the three-valued answer has to be
// reachable and operable from a different package — including Claim.Free, which
// is the one-line form every caller that only asks "may I proceed?" uses.

package cartridge_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
)

// A cartridge nobody has booted is free, and asking gains it no state: the
// probe must never create the lock file it looks for.
func TestBusyOnAnUnbootedCartridgeIsFree(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "demo"+cartridge.SparseExt)

	claim := cartridge.Busy(image)
	if claim.State != cartridge.ClaimFree || !claim.Free() {
		t.Fatalf("Busy(%q) = %+v, want %q", image, claim, cartridge.ClaimFree)
	}
	if claim.Err != nil {
		t.Errorf("Claim.Err = %v, want nil for a free cartridge", claim.Err)
	}
	if !cartridge.Busy("").Free() {
		t.Error("an empty path names no cartridge, so nothing can hold it")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the probe left files beside an unbooted cartridge: %v", entries)
	}
}

// Free is the conservative reading of the three states: only a positive "no
// holder" clears a boot, so both of the other two answers hold it back.
func TestClaimFreeIsTrueOnlyForClaimFree(t *testing.T) {
	for state, want := range map[cartridge.ClaimState]bool{
		cartridge.ClaimFree:          true,
		cartridge.ClaimHeld:          false,
		cartridge.ClaimIndeterminate: false,
	} {
		if got := (cartridge.Claim{State: state}).Free(); got != want {
			t.Errorf("Claim{State: %q}.Free() = %v, want %v", state, got, want)
		}
	}
	// The zero Claim is not a free one by accident of the empty string: an
	// unset state is not an answer.
	if (cartridge.Claim{}).Free() {
		t.Error("a zero Claim must not read as free")
	}
}
