//go:build darwin

package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// entitlementProbeTimeout bounds the codesign call. It reads a signature off
// disk and returns in milliseconds; the bound only exists so a wedged codesign
// cannot stall a start.
const entitlementProbeTimeout = 10 * time.Second

// ErrMissingVirtualizationEntitlement reports a binary that Virtualization.framework
// will refuse to start a VM for. It carries the same guidance the post-hoc VZ
// error does, so both paths tell a user the same thing.
var ErrMissingVirtualizationEntitlement = errors.New(signingHint)

// CheckSelfEntitlement reports whether THIS binary can start a VM at all.
//
// It exists because the answer was previously discovered too late and in the
// wrong place. An unsigned `br` spawns a holder, the holder asks VZ for a
// machine, VZ refuses for want of the entitlement, and that error — which
// already carries the right advice — is written to a log file in the state
// directory. The terminal meanwhile sits watching for a boot stage that will
// never be published, for the length of the start budget. The user sees a
// cursor.
//
// Checked here, before anything is spawned, the same fact costs milliseconds
// and arrives as a sentence.
//
// It FAILS OPEN. codesign missing, unreadable, or slow means "cannot tell", and
// refusing to start on a question we could not answer would be worse than the
// hang this replaces — VZ still makes the real decision, and still explains
// itself when it says no.
func CheckSelfEntitlement(ctx context.Context) error {
	exe, err := os.Executable()
	if err != nil {
		// Deliberate fail-open: not knowing our own path is a "cannot
		// tell", and VZ still makes the real decision.
		return nil //nolint:nilerr // see above
	}
	if hasEntitlement(ctx, exe) {
		return nil
	}
	return ErrMissingVirtualizationEntitlement
}

// hasEntitlement reports whether bin carries the virtualization entitlement,
// answering true whenever it cannot tell.
func hasEntitlement(ctx context.Context, bin string) bool {
	codesign, err := exec.LookPath("codesign")
	if err != nil {
		return true // no codesign on this host; not our question to answer.
	}

	ctx, cancel := context.WithTimeout(ctx, entitlementProbeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, codesign, "-d", "--entitlements", "-", "--xml", bin).CombinedOutput()
	if err != nil {
		return true // unsigned-in-an-unexpected-way, or codesign failed; let VZ decide.
	}
	return strings.Contains(string(out), virtualizationEntitlement)
}
