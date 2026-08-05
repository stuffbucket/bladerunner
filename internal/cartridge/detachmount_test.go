// The export test for DetachMount: it is the entry point a caller outside this
// package uses to release a volume it attached, and the precondition every
// unlink of a cartridge image is gated on, so it has to be reachable and
// operable from a different package.

package cartridge_test

import (
	"errors"
	"runtime"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
)

// A Mount that describes nothing needs no hdiutil and no macOS: there is
// nothing to release, and that is the confirmed answer rather than a refusal.
// It matters because `br disk pack` calls this on a Mount it may already have
// released, and a spurious error there would be joined onto a successful pack.
func TestDetachMountOfNothingIsConfirmed(t *testing.T) {
	if err := cartridge.DetachMount(cartridge.Mount{}); err != nil {
		t.Fatalf("DetachMount(zero Mount) = %v, want nil", err)
	}
	// A Path alone still describes no attachment: nothing was mounted and no
	// device was produced.
	if err := cartridge.DetachMount(cartridge.Mount{Path: "/images/demo" + cartridge.SparseExt}); err != nil {
		t.Fatalf("DetachMount(image path only) = %v, want nil", err)
	}
}

// Off darwin there is no hdiutil to ask, so a Mount that DOES describe an
// attachment cannot be confirmed released and must say so rather than return
// the nil a caller would read as permission to unlink.
func TestDetachMountIsUnsupportedOffDarwin(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("hdiutil exists here; the unsupported path is not reachable")
	}
	err := cartridge.DetachMount(cartridge.Mount{
		Path:       "/images/demo" + cartridge.SparseExt,
		Mountpoint: "/Volumes/bladerunner-demo",
		DevNode:    "/dev/disk9s1",
	})
	if !errors.Is(err, cartridge.ErrUnsupported) {
		t.Fatalf("DetachMount off darwin = %v, want ErrUnsupported", err)
	}
}
