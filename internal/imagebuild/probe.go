package imagebuild

import (
	"context"
	"os"
	"runtime"
)

// Probe gathers what this host can actually do, for building targetArch.
//
// The libguestfs check launches a real appliance and costs seconds, so it runs
// only when it can change the outcome: when the native path is already ruled
// out, or when the operator asked for the appliance explicitly. On a Linux
// builder with root and a loop device the common case pays nothing.
//
// This distinction is the point of the probe. `command -v guestfish` succeeding
// does not mean the appliance launches — an installed-but-broken libguestfs is
// precisely the failure that made every prior guest-image build fail, so
// presence is not accepted as evidence of function.
func Probe(ctx context.Context, want Method, targetArch string) Capabilities {
	caps := Capabilities{
		GOOS:       runtime.GOOS,
		HostArch:   runtime.GOARCH,
		Elevated:   os.Geteuid() == 0,
		LoopDevice: loopDeviceAvailable(),
		VMUsable:   vmRuntimeUsable(),
	}

	if shouldProbeAppliance(want, targetArch, caps) {
		caps.ApplianceUsable = applianceUsable(ctx)
	}
	return caps
}
