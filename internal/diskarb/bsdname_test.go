package diskarb

import "testing"

// The BSD-name rule is stated once, so it is tested once — as a rule, over
// every spelling macOS uses, rather than per caller. Four packages used to
// carry their own reduction and they disagreed; this table is what they all
// now answer to.
func TestBSDNameRuleOverEverySpelling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		// bare is what DiskArbitration matches on, unit the whole-disk unit,
		// path the /dev form recorded on disk.
		bare string
		unit string
		path string
	}{
		{name: "bare slice", in: "disk4s1", bare: "disk4s1", unit: "disk4", path: "/dev/disk4s1"},
		{name: "bare whole disk", in: "disk4", bare: "disk4", unit: "disk4", path: "/dev/disk4"},
		{name: "block device path", in: "/dev/disk4s1", bare: "disk4s1", unit: "disk4", path: "/dev/disk4s1"},
		{name: "whole disk path", in: "/dev/disk4", bare: "disk4", unit: "disk4", path: "/dev/disk4"},
		{name: "raw device path", in: "/dev/rdisk4s1", bare: "disk4s1", unit: "disk4", path: "/dev/disk4s1"},
		{name: "raw whole disk path", in: "/dev/rdisk4", bare: "disk4", unit: "disk4", path: "/dev/disk4"},
		{name: "bare raw name", in: "rdisk4s1", bare: "disk4s1", unit: "disk4", path: "/dev/disk4s1"},
		{name: "apfs sub slice", in: "disk4s1s2", bare: "disk4s1s2", unit: "disk4", path: "/dev/disk4s1s2"},
		{name: "apfs sub slice path", in: "/dev/disk4s1s2", bare: "disk4s1s2", unit: "disk4", path: "/dev/disk4s1s2"},
		{name: "multi digit unit", in: "/dev/disk12s3", bare: "disk12s3", unit: "disk12", path: "/dev/disk12s3"},
		{name: "surrounding space", in: "  /dev/disk4s1  ", bare: "disk4s1", unit: "disk4", path: "/dev/disk4s1"},
		{name: "empty", in: "", bare: "", unit: "", path: ""},
		{name: "prefix with no unit number", in: "disk", bare: "", unit: "", path: ""},
		{name: "unrelated device", in: "/dev/null", bare: "", unit: "", path: ""},
		{name: "mountpoint", in: "/Volumes/bladerunner-demo", bare: "", unit: "", path: ""},
		{name: "relative path that merely starts with disk", in: "diskimages/demo.dmg", bare: "", unit: "", path: ""},
		{name: "garbage", in: "not-a-device", bare: "", unit: "", path: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := BSDName(tt.in); got != tt.bare {
				t.Errorf("BSDName(%q) = %q, want %q", tt.in, got, tt.bare)
			}
			if got := WholeDiskName(tt.in); got != tt.unit {
				t.Errorf("WholeDiskName(%q) = %q, want %q", tt.in, got, tt.unit)
			}
			if got := DevPath(tt.in); got != tt.path {
				t.Errorf("DevPath(%q) = %q, want %q", tt.in, got, tt.path)
			}
		})
	}
}

// The three helpers are one rule seen from three sides, so they must agree with
// each other on every input: the path form must re-reduce to the same bare name
// and the same unit, and the unit must itself be a valid bare name.
func TestBSDNameHelpersAreIdempotent(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"disk4s1", "/dev/disk4s1", "/dev/rdisk4", "disk4s1s2", "/dev/disk12"} {
		bare, unit, path := BSDName(in), WholeDiskName(in), DevPath(in)
		if got := BSDName(path); got != bare {
			t.Errorf("BSDName(DevPath(%q)) = %q, want %q", in, got, bare)
		}
		if got := WholeDiskName(path); got != unit {
			t.Errorf("WholeDiskName(DevPath(%q)) = %q, want %q", in, got, unit)
		}
		if got := BSDName(unit); got != unit {
			t.Errorf("BSDName(WholeDiskName(%q)) = %q, want %q", in, got, unit)
		}
	}
}

// DevDir is exported because callers render device paths with it; it must stay
// the directory the kernel actually uses, since DevPath's output is compared
// against records written by hdiutil and diskutil.
func TestDevPathUsesTheKernelDeviceDirectory(t *testing.T) {
	t.Parallel()

	if DevDir != "/dev/" {
		t.Fatalf("DevDir = %q, want /dev/", DevDir)
	}
	if got := DevPath("disk4s1"); got != DevDir+"disk4s1" {
		t.Fatalf("DevPath = %q, want %q", got, DevDir+"disk4s1")
	}
}
