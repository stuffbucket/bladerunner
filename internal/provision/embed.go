package provision

import (
	_ "embed"
	"strconv"
	"strings"

	"github.com/stuffbucket/bladerunner/internal/config"
)

// Time-heal provisioning assets, embedded from the checked-in files under
// internal/provision/scripts/. Those files are the SINGLE SOURCE OF TRUTH: the
// cloud-init path (renderTimeHeal in cloudinit.go) emits these bytes verbatim,
// and the image-build path (scripts/build-guest-image.sh) --copy-ins the very
// same files. There is no separate Go-const twin to keep in sync, so no
// byte-identity guard test is needed.
//
// The embed paths are relative to this package directory and cannot traverse
// upward, which is why the assets live in internal/provision/scripts/ rather
// than the repo-root scripts/.

// watchdogScript is the guest-local wake-heal watchdog body. Emitted verbatim by
// the cloud-init path and --copy-in'd by the image build. Port values are
// threaded via the /etc/default/bladerunner-watchdog env file (written by
// renderTimeHeal), NOT by substitution into this body.
//
//go:embed scripts/bladerunner-watchdog.sh
var watchdogScript string

// watchdogUnit is the systemd unit for the watchdog.
//
//go:embed scripts/bladerunner-watchdog.service
var watchdogUnit string

// chronyConf is the suspend-tuned chrony.conf written to /etc/chrony/chrony.conf
// in the cloud-init path. makestep 1.0 -1 steps the clock for ANY offset >1s an
// UNLIMITED number of times — the guest-local recovery for a host suspend (no
// paravirt "you were stopped" signal exists).
//
//go:embed scripts/chrony.conf
var chronyConf string

// vsockNTPUnitTemplate is the bladerunner-vsock-ntp.service unit body. The
// checked-in file bakes config.DefaultVsockNTPPort as the literal VSOCK-CONNECT
// port; vsockNTPUnit re-templates that literal to the configured port.
//
//go:embed scripts/bladerunner-vsock-ntp.service
var vsockNTPUnitTemplate string

// vsockNTPUnit renders the bladerunner-vsock-ntp.service unit body with the
// given vsock port. The checked-in template bakes config.DefaultVsockNTPPort as
// the VSOCK-CONNECT target, so rendering at the default port returns the file
// bytes unchanged; any other port swaps only that one literal.
func vsockNTPUnit(port uint32) string {
	defaultTarget := "VSOCK-CONNECT:2:" + strconv.Itoa(config.DefaultVsockNTPPort)
	wantTarget := "VSOCK-CONNECT:2:" + strconv.FormatUint(uint64(port), 10)
	return strings.Replace(vsockNTPUnitTemplate, defaultTarget, wantTarget, 1)
}
