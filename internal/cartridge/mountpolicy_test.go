package cartridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// browsablePlist renders what `hdiutil attach -plist` prints for a cartridge
// mounted browsably: the whole disk, the APFS container, and the one volume
// macOS placed under /Volumes.
func browsablePlist(mountpoint, devNode string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array>
		<dict>
			<key>content-hint</key>
			<string>GUID_partition_scheme</string>
			<key>dev-entry</key>
			<string>/dev/disk12</string>
		</dict>
		<dict>
			<key>content-hint</key>
			<string>Apple_APFS</string>
			<key>dev-entry</key>
			<string>/dev/disk12s1</string>
		</dict>
		<dict>
			<key>dev-entry</key>
			<string>%s</string>
			<key>mount-point</key>
			<string>%s</string>
			<key>volume-kind</key>
			<string>apfs</string>
		</dict>
	</array>
</dict>
</plist>
`, devNode, mountpoint)
}

// unmountedPlist is a successful attach that mounted nothing — the case a
// browsable attach cannot recover from, because it dictated no location.
const unmountedPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array>
		<dict><key>dev-entry</key><string>/dev/disk12</string></dict>
		<dict><key>dev-entry</key><string>/dev/disk12s1</string></dict>
	</array>
</dict>
</plist>
`

// nonDictEntitiesPlist is a parseable attach plist whose system-entities array
// holds values that are not dictionaries, so nothing usable can be read out of
// it — and an empty one, which is the same dead end reached a different way.
const nonDictEntitiesPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array>
		<string>/dev/disk12</string>
		<integer>7</integer>
	</array>
</dict>
</plist>
`

const emptyEntitiesPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array/>
</dict>
</plist>
`

// noDevEntryPlist parses and names entities, but not one of them carries a
// dev-entry, so the attach cannot clean itself up out of its own output.
const noDevEntryPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array>
		<dict><key>content-hint</key><string>GUID_partition_scheme</string></dict>
		<dict><key>volume-kind</key><string>apfs</string></dict>
	</array>
</dict>
</plist>
`

func TestMountPolicyZeroValueIsBrowsable(t *testing.T) {
	var zero MountPolicy
	if zero.Resolve() != MountBrowsable {
		t.Fatalf("zero policy resolved to %q, want %q", zero.Resolve(), MountBrowsable)
	}
	if !zero.Browsable() || zero.Private() {
		t.Fatalf("zero policy: Browsable=%v Private=%v", zero.Browsable(), zero.Private())
	}
	if zero.String() != string(MountBrowsable) {
		t.Fatalf("zero policy String = %q", zero.String())
	}
	if DefaultMountPolicy != MountBrowsable {
		t.Fatalf("DefaultMountPolicy = %q, want %q", DefaultMountPolicy, MountBrowsable)
	}
}

func TestMountPolicyPredicates(t *testing.T) {
	tests := []struct {
		policy    MountPolicy
		browsable bool
		private   bool
		valid     bool
	}{
		{policy: "", browsable: true, valid: true},
		{policy: MountBrowsable, browsable: true, valid: true},
		{policy: MountPrivate, private: true, valid: true},
		{policy: "nonsense"},
	}
	for _, tc := range tests {
		t.Run(string(tc.policy), func(t *testing.T) {
			if got := tc.policy.Browsable(); got != tc.browsable {
				t.Errorf("Browsable() = %v, want %v", got, tc.browsable)
			}
			if got := tc.policy.Private(); got != tc.private {
				t.Errorf("Private() = %v, want %v", got, tc.private)
			}
			if got := tc.policy.Valid(); got != tc.valid {
				t.Errorf("Valid() = %v, want %v", got, tc.valid)
			}
		})
	}
}

func TestMountPolicyForFlag(t *testing.T) {
	if got := MountPolicyFor(true); got != MountPrivate {
		t.Errorf("MountPolicyFor(true) = %q, want %q", got, MountPrivate)
	}
	if got := MountPolicyFor(false); got != MountBrowsable {
		t.Errorf("MountPolicyFor(false) = %q, want %q", got, MountBrowsable)
	}
}

// TestAttachArgsBrowsableDictatesNothing is the whole inversion in one
// assertion: no -mountpoint (so macOS may suffix a colliding name instead of
// failing) and no -nobrowse (so the volume is ejectable in Finder).
func TestAttachArgsBrowsableDictatesNothing(t *testing.T) {
	got := attachArgs(attachRequest{path: "/images/demo" + DMGExt, policy: MountBrowsable})
	want := []string{
		"attach", "/images/demo" + DMGExt,
		"-owners", "on",
		"-noverify",
		"-plist",
	}
	if !argsEqual(got, want) {
		t.Fatalf("browsable attachArgs mismatch\n got: %v\nwant: %v", got, want)
	}
	// The zero value must produce the identical vector, since it IS browsable.
	if zero := attachArgs(attachRequest{path: "/images/demo" + DMGExt}); !argsEqual(zero, want) {
		t.Fatalf("zero-policy attachArgs = %v, want the browsable vector", zero)
	}
	// A mountpoint set alongside a browsable policy is ignored, never passed:
	// callers such as vmhost hand one over unconditionally.
	hinted := attachArgs(attachRequest{path: "/images/demo" + DMGExt, mountpoint: "/state/mnt/demo", policy: MountBrowsable})
	if !argsEqual(hinted, want) {
		t.Fatalf("browsable attach leaked a mountpoint: %v", hinted)
	}
}

// TestAttachBrowsableReadsTheMountpointBack is the behavior that makes
// collision suffixing work: the mountpoint comes from hdiutil's plist, and is
// NOT assumed from the cartridge name.
func TestAttachBrowsableReadsTheMountpointBack(t *testing.T) {
	const (
		devNode  = "/dev/disk12s2"
		mounted  = VolumesRoot + "/bladerunner-demo 1"
		imagePth = "/images/demo" + SparseExt
	)
	f := &fakeRunner{results: []fakeResult{{stdout: browsablePlist(mounted, devNode)}}}

	m, err := attach(context.Background(), f, attachRequest{path: imagePth, policy: MountBrowsable})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if m.Mountpoint != mounted {
		t.Errorf("Mountpoint = %q, want the plist's %q", m.Mountpoint, mounted)
	}
	if m.Mountpoint == BrowsableMountpointFor("demo") {
		t.Error("the collision-suffixed mountpoint must not be the predicted one")
	}
	if m.DevNode != devNode {
		t.Errorf("DevNode = %q, want %q", m.DevNode, devNode)
	}
	if m.Policy != MountBrowsable {
		t.Errorf("Policy = %q, want %q", m.Policy, MountBrowsable)
	}
	// The suffixed volume must still resolve back to the cartridge name, or
	// mount detection would offer to boot "demo 1".
	if got := NameFromVolume(m.Mountpoint); got != "demo" {
		t.Errorf("NameFromVolume(%q) = %q, want demo", m.Mountpoint, got)
	}
	if len(f.calls) != 1 {
		t.Fatalf("hdiutil calls = %v, want a single attach", f.calls)
	}
	for _, arg := range f.calls[0] {
		if arg == flagMountpoint || arg == flagNoBrowse {
			t.Fatalf("browsable attach passed %q: %v", arg, f.calls[0])
		}
	}
}

// TestAttachBrowsableCreatesNoDirectory guards against the private path's
// MkdirAll leaking into the browsable one — creating a stray directory where
// macOS is about to mount would be actively harmful.
func TestAttachBrowsableCreatesNoDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "should-not-exist")
	f := &fakeRunner{results: []fakeResult{{stdout: browsablePlist(VolumesRoot+"/bladerunner-demo", "/dev/disk12s2")}}}

	if _, err := attach(context.Background(), f, attachRequest{
		path:       "/images/demo" + SparseExt,
		mountpoint: target,
		policy:     MountBrowsable,
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("browsable attach created %s: %v", target, err)
	}
}

// TestAttachBrowsableFailsWithoutAMountpoint pins the asymmetry with the private
// policy: there the plist is additive (we dictated the location), here it is the
// only source of truth, so an unusable plist is fatal — and whatever did attach
// is unwound rather than stranded.
//
// The unwind may NOT depend on the malformed output naming the device needed to
// clean itself up. Every row below is a successful attach whose description is
// useless in a different way; each must still end with the image released.
func TestAttachBrowsableFailsWithoutAMountpoint(t *testing.T) {
	const image = "/images/demo" + SparseExt
	// What `hdiutil info` reports when asked which device serves the image the
	// attach just succeeded on — the recovery handle a malformed attach cannot
	// take away.
	recovered := fakeResult{stdout: infoPlistFor("/Volumes/bladerunner-demo", "/dev/disk12s2", image, true)}
	nothing := fakeResult{stdout: emptyInfoPlist}

	tests := []struct {
		name       string
		stdout     string
		recovery   *fakeResult
		wantDetach string
	}{
		{
			name:       "attached but mounted nothing",
			stdout:     unmountedPlist,
			wantDetach: "/dev/disk12", // the whole disk, straight out of the plist
		},
		{
			name:       "output is not a plist at all",
			stdout:     "/dev/disk12\tGUID_partition_scheme\t\n",
			recovery:   &recovered,
			wantDetach: "/dev/disk12s2",
		},
		{
			name:       "system-entities holds no dictionaries",
			stdout:     nonDictEntitiesPlist,
			recovery:   &recovered,
			wantDetach: "/dev/disk12s2",
		},
		{
			name:       "system-entities is empty",
			stdout:     emptyEntitiesPlist,
			recovery:   &recovered,
			wantDetach: "/dev/disk12s2",
		},
		{
			name:       "no entity carries a dev-entry",
			stdout:     noDevEntryPlist,
			recovery:   &recovered,
			wantDetach: "/dev/disk12s2",
		},
		{
			name:     "the attach left nothing attached after all",
			stdout:   emptyEntitiesPlist,
			recovery: &nothing,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			results := []fakeResult{{stdout: tc.stdout}}
			if tc.recovery != nil {
				results = append(results, *tc.recovery)
			}
			f := &fakeRunner{results: results}
			_, err := attach(context.Background(), f, attachRequest{path: image, policy: MountBrowsable})
			if !errors.Is(err, ErrMountpointUnknown) {
				t.Fatalf("err = %v, want ErrMountpointUnknown", err)
			}
			verbs := hdiutilVerbs(f)
			if tc.wantDetach == "" {
				// hdiutil confirmed nothing is attached from the image, so there
				// is nothing to release and no detach to run.
				if len(verbs) != 2 || verbs[1] != cmdInfo {
					t.Fatalf("hdiutil calls = %v, want the attach and the probe alone", f.calls)
				}
				return
			}
			last := f.lastCall()
			if len(last) < 3 || last[1] != cmdDetach || last[2] != tc.wantDetach {
				t.Fatalf("hdiutil calls = %v, want the attach unwound by detach %s", f.calls, tc.wantDetach)
			}
		})
	}
}

// A successful attach that can be neither described nor released is the one
// outcome the user has to be told about twice: the cartridge is unusable AND a
// volume nothing owns is still on their machine.
func TestAttachBrowsableReportsAnUnwindItCouldNotConfirm(t *testing.T) {
	const image = "/images/demo" + SparseExt
	f := &fakeRunner{results: []fakeResult{
		{stdout: emptyEntitiesPlist},                                // attached, but nothing usable to address
		{stderr: "hdiutil: info failed", err: errors.New("exit 1")}, // and the recovery probe fails
	}}

	_, err := attach(context.Background(), f, attachRequest{path: image, policy: MountBrowsable})
	if !errors.Is(err, ErrMountpointUnknown) {
		t.Fatalf("err = %v, want it to keep reporting ErrMountpointUnknown", err)
	}
	for _, want := range []string{image, "hdiutil detach"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q, so the user cannot clear the stranded volume", err, want)
		}
	}
}

// TestAttachBrowsableSurfacesTheHdiutilError keeps the failure message the same
// shape as the private path's.
func TestAttachBrowsableSurfacesTheHdiutilError(t *testing.T) {
	f := &fakeRunner{results: []fakeResult{{stderr: "hdiutil: attach failed - no mountable file systems", err: errors.New("exit 1")}}}
	_, err := attach(context.Background(), f, attachRequest{path: "/images/x" + DMGExt, policy: MountBrowsable})
	if err == nil || !strings.Contains(err.Error(), "no mountable file systems") {
		t.Fatalf("err = %v, want hdiutil's stderr", err)
	}
	if errors.Is(err, ErrMountpointUnknown) {
		t.Fatal("an hdiutil failure must not be reported as an unknown mountpoint")
	}
}

func TestSelectMountedEntity(t *testing.T) {
	tests := []struct {
		name     string
		entities []systemEntity
		want     string
		found    bool
	}{
		{
			name: "the one volume with a mount-point wins",
			entities: []systemEntity{
				{DevEntry: "/dev/disk12"},
				{DevEntry: "/dev/disk12s1"},
				{DevEntry: "/dev/disk12s2", MountPoint: "/Volumes/bladerunner-demo", VolumeKind: "apfs"},
			},
			want:  "/dev/disk12s2",
			found: true,
		},
		{
			name:     "nothing mounted",
			entities: []systemEntity{{DevEntry: "/dev/disk12"}},
		},
		{name: "no entities"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectMountedEntity(tc.entities)
			if ok != tc.found {
				t.Fatalf("found = %v, want %v", ok, tc.found)
			}
			if got.DevEntry != tc.want {
				t.Fatalf("DevEntry = %q, want %q", got.DevEntry, tc.want)
			}
		})
	}
}

// TestBrowsableMountpointForIsAPrediction documents the contract: it names where
// macOS *usually* puts the volume, and is deliberately not what attach relies
// on. It is also the reason pack and boot cannot collide — one is under
// /Volumes, the other under the state dir.
func TestBrowsableMountpointForIsAPrediction(t *testing.T) {
	got := BrowsableMountpointFor("demo")
	if got != VolumesRoot+"/bladerunner-demo" {
		t.Fatalf("BrowsableMountpointFor = %q", got)
	}
	if !IsCandidate(got) {
		t.Errorf("the predicted mountpoint %q must pass the detection prefilter", got)
	}
	if priv := MountpointFor("/state", "demo"); priv == got {
		t.Fatalf("pack's private mountpoint %q collides with boot's %q", priv, got)
	}
	if strings.HasPrefix(MountpointFor("/state", "demo"), VolumesRoot) {
		t.Error("the private mountpoint must stay off /Volumes so pack never contends with a booted cartridge")
	}
}
