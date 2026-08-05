//go:build linux

package imagebuild

import (
	"os"
	"os/exec"
)

// nbdDevicePath is the first network-block-device node. The mechanic connects
// the image to this device, so its presence is what makes a bake viable.
//
// It is a variable so a test can point it at a path it controls and exercise
// both outcomes without depending on the kernel the tests run under.
var nbdDevicePath = "/dev/nbd0"

// haveQemuNBD reports whether the qemu-nbd binary is present. A variable for
// the same reason.
var haveQemuNBD = func() bool {
	_, err := exec.LookPath("qemu-nbd")
	return err == nil
}

// nativeAttachAvailable reports whether the image can be attached as a block
// device by the means the mechanic actually uses.
//
// It deliberately checks the nbd device and qemu-nbd rather than a loop device.
// The two are not interchangeable, and this project measured a host where they
// disagreed: a container exposing /dev/loop-control could not load the nbd
// module at all. Probing the loop device there would have accepted a host on
// which the build then fails part-way through, after downloading and resizing
// the image.
//
// The device node only exists once the nbd module is loaded, so a host that
// could bake after a modprobe still reports false here. That is a refusal an
// operator can act on, which is the safe direction: the alternative is starting
// a build that fails minutes in.
func nativeAttachAvailable() bool {
	if !haveQemuNBD() {
		return false
	}
	f, err := os.Open(nbdDevicePath)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
