package imagebuild

import (
	"os"
	"runtime"
)

// Probe gathers what this host can actually do, for building targetArch.
//
// There is one mechanic, so this is a small, cheap set of facts: no capability
// here costs more than a syscall. The package previously ran a real libguestfs
// appliance launch as part of probing, because presence of a binary is not
// evidence that it works — a true principle that no longer has a subject, since
// the appliance mechanic it guarded was never written.
func Probe(targetArch string) Capabilities {
	return Capabilities{
		GOOS:         runtime.GOOS,
		HostArch:     runtime.GOARCH,
		Elevated:     os.Geteuid() == 0,
		NativeAttach: nativeAttachAvailable(),
	}
}
