package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/imagebuild"
)

// The probe establishes that libguestfs works only because it applies specific
// backend settings. If those settings do not also reach the build subprocess,
// the probe and the build disagree: the probe boots an appliance successfully
// and the build then fails on the exact aarch64 defect the settings exist to
// work around. Reported as point 3 of #239.
func TestBuildEnvCarriesApplianceSettingsToTheBuild(t *testing.T) {
	env := buildEnv(nil, imagebuild.MethodAppliance)

	for _, want := range []string{"LIBGUESTFS_BACKEND_SETTINGS=force_tcg", "LIBGUESTFS_BACKEND=direct"} {
		if !slices.Contains(env, want) {
			t.Errorf("buildEnv(appliance) missing %q; the probe applies it but the build would not", want)
		}
	}
}

// The native path never launches an appliance, so loading it with libguestfs
// settings would be misleading to anyone debugging a build.
func TestBuildEnvLeavesTheNativePathAlone(t *testing.T) {
	env := buildEnv([]string{"PATH=/usr/bin"}, imagebuild.MethodNative)

	for _, entry := range env {
		if strings.HasPrefix(entry, "LIBGUESTFS_") {
			t.Errorf("buildEnv(native) set %q; the native path boots no appliance", entry)
		}
	}
	if !slices.Contains(env, "PATH=/usr/bin") {
		t.Error("buildEnv() dropped the inherited environment")
	}
}
