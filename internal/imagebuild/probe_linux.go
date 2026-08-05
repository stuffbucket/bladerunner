//go:build linux

package imagebuild

import (
	"os"
	"os/exec"
	"strconv"
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

// loadNBDModule asks the kernel for the nbd driver, with the same options the
// mechanic uses — a probe that loaded it differently would answer about a
// configuration the build then does not get. A variable so a test can exercise
// both outcomes without needing root or a real module.
var loadNBDModule = func() error {
	return exec.Command("modprobe", "nbd", "max_part="+strconv.Itoa(nbdMaxPart)).Run()
}

// nativeAttachAvailable reports whether the image can be attached as a block
// device by the means the mechanic actually uses.
//
// It checks nbd and qemu-nbd rather than a loop device. The two are not
// interchangeable, and this project measured a host where they disagreed: a
// container exposing /dev/loop-control could not load the nbd module at all.
// Probing the loop device there would have accepted a host on which the build
// then fails part-way through, after downloading and resizing the image.
//
// A MISSING DEVICE NODE IS NOT AN ANSWER. The node appears only once the module
// is loaded, and the mechanic loads it itself, so refusing on absence rejects
// hosts that can bake perfectly well — every fresh CI runner among them. That
// used to be harmless: the refusal fell back to the libguestfs appliance, and
// the old comment here called it "the safe direction to be wrong in". Deleting
// the appliance removed the fallback and turned it into a failed release build.
//
// So the module is loaded here, and the probe answers on the result. It is the
// same modprobe the mechanic runs and is idempotent, which is what makes it
// acceptable to do inside a probe: this reports what the executor will find,
// not what happens to be materialized at the moment of asking.
func nativeAttachAvailable() bool {
	if !haveQemuNBD() {
		return false
	}
	if nbdDeviceExists() {
		return true
	}
	if err := loadNBDModule(); err != nil {
		return false
	}
	return nbdDeviceExists()
}

// nbdDeviceExists reports whether the nbd device node is present.
func nbdDeviceExists() bool {
	f, err := os.Open(nbdDevicePath)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
