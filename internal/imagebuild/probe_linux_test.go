//go:build linux

package imagebuild

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubAttachProbe points the probe at a device path the test controls and a
// modprobe it can script, and restores both afterwards.
func stubAttachProbe(t *testing.T, device string, qemuNBD bool, load func() error) {
	t.Helper()

	oldDevice, oldQemu, oldLoad := nbdDevicePath, haveQemuNBD, loadNBDModule
	t.Cleanup(func() { nbdDevicePath, haveQemuNBD, loadNBDModule = oldDevice, oldQemu, oldLoad })

	nbdDevicePath = device
	haveQemuNBD = func() bool { return qemuNBD }
	loadNBDModule = load
}

// A fresh host has qemu-nbd but no device node, because the node appears only
// once the nbd module is loaded. The probe must load it and answer on the
// result, not refuse.
//
// This is the regression that broke the release build. The probe reported false
// on every fresh CI runner, which was survivable while a refusal fell back to
// the libguestfs appliance — and became fatal the moment that fallback was
// deleted, because nothing was left to fall back to.
func TestNativeAttachLoadsTheModuleBeforeAnswering(t *testing.T) {
	device := filepath.Join(t.TempDir(), "nbd0")
	loaded := false

	stubAttachProbe(t, device, true, func() error {
		loaded = true
		// Loading the module is what makes the node appear.
		return os.WriteFile(device, nil, 0o600)
	})

	if !nativeAttachAvailable() {
		t.Error("refused a host whose nbd module simply was not loaded yet")
	}
	if !loaded {
		t.Error("never attempted to load the nbd module")
	}
}

// A host where the module cannot load has no attach, and must say so. This is
// the container case that made the probe check nbd rather than a loop device.
func TestNativeAttachRefusesWhenTheModuleWillNotLoad(t *testing.T) {
	device := filepath.Join(t.TempDir(), "absent")

	stubAttachProbe(t, device, true, func() error { return errors.New("no such module") })

	if nativeAttachAvailable() {
		t.Error("accepted a host where the nbd module cannot load")
	}
}

// A module that loads but produces no device node is still no attach. modprobe
// can succeed on a kernel whose nbd support is built differently, and the
// mechanic needs the node, not the module.
func TestNativeAttachRefusesWhenLoadingProducesNoDevice(t *testing.T) {
	device := filepath.Join(t.TempDir(), "never-appears")

	stubAttachProbe(t, device, true, func() error { return nil })

	if nativeAttachAvailable() {
		t.Error("accepted a host where modprobe succeeded but no device appeared")
	}
}

// Without qemu-nbd there is nothing to attach with, and the module must not be
// loaded speculatively — a probe should not change a host it is going to refuse.
func TestNativeAttachRefusesWithoutQemuNBDAndLoadsNothing(t *testing.T) {
	device := filepath.Join(t.TempDir(), "nbd0")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatalf("seed the device node: %v", err)
	}
	loaded := false

	stubAttachProbe(t, device, false, func() error { loaded = true; return nil })

	if nativeAttachAvailable() {
		t.Error("accepted a host with no qemu-nbd")
	}
	if loaded {
		t.Error("loaded a kernel module on a host it was going to refuse anyway")
	}
}

// An already-loaded module must not be reloaded. The common case is a builder
// that has baked before, and a probe that runs modprobe every time is doing
// work it does not need to.
func TestNativeAttachSkipsTheModprobeWhenTheDeviceIsThere(t *testing.T) {
	device := filepath.Join(t.TempDir(), "nbd0")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatalf("seed the device node: %v", err)
	}
	loaded := false

	stubAttachProbe(t, device, true, func() error { loaded = true; return nil })

	if !nativeAttachAvailable() {
		t.Error("refused a host whose device node is already present")
	}
	if loaded {
		t.Error("ran modprobe despite the device already existing")
	}
}
