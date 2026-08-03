package imagebuild

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// sha512HexLength is the length of a SHA-512 digest written as hex: 64 bytes,
// two characters each. A pin of any other length is a truncated paste.
const sha512HexLength = 128

// basePinsFileName is used by the bake and by the tests above, but
// //go:embed needs a string literal and cannot take the constant. That leaves
// two spellings of one filename, which is the drift this package exists to end.
// Reading the file by the constant's name proves they still agree.
func TestBasePinsFileNameMatchesTheEmbed(t *testing.T) {
	onDisk, err := os.ReadFile(basePinsFileName)
	if err != nil {
		t.Fatalf("read %s by its constant: %v", basePinsFileName, err)
	}
	if string(onDisk) != basePinsRaw {
		t.Errorf("%s does not match the embedded copy; the //go:embed directive names a different file", basePinsFileName)
	}
}

// The set of architectures the build supports must be DERIVED from the pins,
// not listed beside them.
//
// A hand-written list is a check that can only ever cover what existed when it
// was written: adding an architecture means editing the pin file and every
// enumeration of it, and nothing fails when one is missed. That is the same
// defect as a lint job that analyses one build, or a test that iterates the
// stages it already knows — the failure is always an addition, and an
// enumeration cannot see additions.
//
// So BaseRelease answers "is this architecture supported" by asking whether it
// is pinned. Adding one becomes a single edit to basepins.sha512, and an
// architecture that is not pinned cannot be built by construction rather than
// by remembering.
func TestBaseReleaseSupportsExactlyThePinnedArches(t *testing.T) {
	saved := basePins
	t.Cleanup(func() { basePins = saved })

	// A pin set naming one architecture that no hardcoded list ever mentioned.
	basePins = map[string]string{
		fmt.Sprintf("debian-%s-%s-riscv64-%s.qcow2", debianRelease, debianVariant, debianStamp): strings.Repeat("a", sha512HexLength),
	}

	if _, err := BaseRelease("riscv64"); err != nil {
		t.Errorf("BaseRelease rejected riscv64 although it is pinned: %v", err)
	}
	if _, err := BaseRelease("arm64"); err == nil {
		t.Error("BaseRelease accepted arm64 although nothing pins it; the supported set is not derived from the pins")
	}
}
