//go:build !linux

package imagebuild

import (
	"fmt"
	"runtime"
)

// HostMechanic reports that this host has none.
//
// It returns an error rather than a mechanic that refuses when called. The
// difference matters: a bake cannot begin, so it cannot fetch 321 MB and resize
// it before discovering there was never anything to run. The message names the
// way out, because the usual reader is on a Mac and the answer is a Linux VM
// they most likely already have.
func HostMechanic(_ string, _ func(string)) (Mechanic, error) {
	return nil, fmt.Errorf("%w: mounting a guest root and chrooting into it needs Linux, but this host is %s; "+
		"build in a Linux VM (colima, lima, UTM) or WSL2, or use the published image from the guest-image-latest release",
		ErrUnsupportedHost, runtime.GOOS)
}
