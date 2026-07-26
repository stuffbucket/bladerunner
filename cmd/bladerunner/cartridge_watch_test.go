package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// The decision function is the whole point of the split: every case below is a
// volume that really can appear on a Mac, and none of them touches a disk.

// testCartridgeMount is the mountpoint every fixture below is rooted at.
const testCartridgeMount = "/Volumes/bladerunner-demo"

// bootableDetected is a Detect result for a healthy, shipped cartridge.
func bootableDetected(name string) *cartridge.Detected {
	return &cartridge.Detected{
		Status:        cartridge.StatusBootable,
		Name:          name,
		Mountpoint:    testCartridgeMount,
		VolumeName:    filepath.Base(testCartridgeMount),
		FormatVersion: cartridge.FormatVersion,
		ReadOnly:      true,
		BackingImage:  "/Users/someone/Downloads/" + name + ".dmg",
		DevNode:       "/dev/disk9s1",
	}
}

// detectReturning builds a detectFunc that always answers with d, recording how
// many times it was consulted.
func detectReturning(d *cartridge.Detected, err error, calls *int) detectFunc {
	return func(string) (*cartridge.Detected, error) {
		if calls != nil {
			*calls++
		}
		return d, err
	}
}

func TestDecideForVolume(t *testing.T) {
	const mount = testCartridgeMount
	cartDisk := diskarb.DiskInfo{
		BSDName:    "disk9s1",
		VolumeName: "bladerunner-demo",
		VolumePath: mount,
		VolumeKind: "apfs",
		Ejectable:  true,
	}

	tests := []struct {
		name        string
		disk        diskarb.DiskInfo
		detected    *cartridge.Detected
		detectErr   error
		held        heldFunc
		wantVerdict watchVerdict
		wantReason  string
		wantSource  string
		wantHeldBy  string
		wantDetects int
	}{
		{
			name:        "unrelated volume is never touched",
			disk:        diskarb.DiskInfo{BSDName: "disk3s2", VolumeName: "Time Machine", VolumePath: "/Volumes/Time Machine"},
			wantVerdict: verdictIgnore,
			wantReason:  reasonNotCandidate,
			wantDetects: 0,
		},
		{
			name:        "unmounted disk is ignored",
			disk:        diskarb.DiskInfo{BSDName: "disk9"},
			wantVerdict: verdictIgnore,
			wantReason:  reasonNoVolume,
		},
		{
			name:        "network share is ignored",
			disk:        diskarb.DiskInfo{BSDName: "disk9s1", VolumeName: "bladerunner-demo", VolumePath: mount, NetworkVolume: true},
			wantVerdict: verdictIgnore,
			wantReason:  reasonNetwork,
		},
		{
			name: "candidate name but no disk.json",
			disk: cartDisk,
			detected: &cartridge.Detected{
				Status: cartridge.StatusNotCartridge, Mountpoint: mount,
				Reason: mount + " has no disk.json at its root",
			},
			wantVerdict: verdictIgnore,
			wantDetects: 1,
		},
		{
			name: "unreadable volume is a permissions problem, not a negative",
			disk: cartDisk,
			detected: &cartridge.Detected{
				Status: cartridge.StatusNotCartridge, Mountpoint: mount,
				Reason: mount + " cannot be inspected",
				Err:    fmt.Errorf("stat %s: %w", mount, fs.ErrPermission),
			},
			wantVerdict: verdictWarn,
			wantReason:  reasonUnreadable,
			wantDetects: 1,
		},
		{
			name: "cartridge missing root.img warns with the reason",
			disk: cartDisk,
			detected: &cartridge.Detected{
				Status: cartridge.StatusUnbootable, Name: "demo", Mountpoint: mount,
				Reason: "incomplete cartridge: missing root.img",
			},
			wantVerdict: verdictWarn,
			wantReason:  "incomplete cartridge: missing root.img",
			wantDetects: 1,
		},
		{
			name: "cartridge from a newer bladerunner warns with the reason",
			disk: cartDisk,
			detected: &cartridge.Detected{
				Status: cartridge.StatusUnbootable, Name: "demo", Mountpoint: mount,
				FormatVersion: cartridge.FormatVersion + 1,
				Reason:        "cartridge format v2 is newer than this bladerunner supports (v1)",
				Err:           cartridge.ErrFormatTooNew,
			},
			wantVerdict: verdictWarn,
			wantReason:  "cartridge format v2 is newer than this bladerunner supports (v1)",
			wantDetects: 1,
		},
		{
			name:        "detect failure is reported, not swallowed",
			disk:        cartDisk,
			detectErr:   errors.New("boom"),
			wantVerdict: verdictWarn,
			wantReason:  "could not be inspected: boom",
			wantDetects: 1,
		},
		{
			name:        "bootable cartridge is offered with its source file",
			disk:        cartDisk,
			detected:    bootableDetected("demo"),
			wantVerdict: verdictOffer,
			wantSource:  "/Users/someone/Downloads/demo.dmg",
			wantDetects: 1,
		},
		{
			name:     "bootable cartridge with no backing image is refused",
			disk:     cartDisk,
			detected: func() *cartridge.Detected { d := bootableDetected("demo"); d.BackingImage = ""; return d }(),

			wantVerdict: verdictWarn,
			wantReason:  reasonNoBackingImage,
			wantDetects: 1,
		},
		{
			name:        "bootable cartridge with an unusable name is refused",
			disk:        cartDisk,
			detected:    bootableDetected("Demo Cartridge"),
			wantVerdict: verdictWarn,
			wantDetects: 1,
		},
		{
			name:        "cartridge we already hold is ignored, not re-offered",
			disk:        cartDisk,
			detected:    bootableDetected("demo"),
			held:        func(heldVolume) (string, bool) { return "demo", true },
			wantVerdict: verdictIgnore,
			wantHeldBy:  "demo",
			wantDetects: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got := decideForVolume(tt.disk, detectReturning(tt.detected, tt.detectErr, &calls), tt.held)

			if got.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q (reason %q)", got.Verdict, tt.wantVerdict, got.Reason)
			}
			if tt.wantReason != "" && got.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.wantReason)
			}
			if got.SourcePath != tt.wantSource {
				t.Errorf("source = %q, want %q", got.SourcePath, tt.wantSource)
			}
			if got.HeldBy != tt.wantHeldBy {
				t.Errorf("heldBy = %q, want %q", got.HeldBy, tt.wantHeldBy)
			}
			if calls != tt.wantDetects {
				t.Errorf("detect called %d times, want %d", calls, tt.wantDetects)
			}
			if got.Verdict == verdictWarn && got.Reason == "" {
				t.Error("a warn with no reason tells the user nothing")
			}
		})
	}
}

// A held cartridge must be recognized when ANY of the three keys matches: a
// holder may have recorded the whole disk while DiskArbitration reports the
// slice, and a second Finder mount of the same file shares neither its
// mountpoint nor its device — only the image behind it.
func TestHeldVolumesAgainstRegistry(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "mnt", "demo")
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "Downloads", "demo.dmg")
	working := filepath.Join(root, "Downloads", "demo.sparseimage")

	live := instance.Entry{
		Name:        "demo",
		Kind:        instance.KindCartridge,
		StateDir:    mount,
		Mountpoint:  mount,
		DevNode:     "/dev/disk9",
		SourcePath:  source,
		WorkingCopy: working,
		PID:         os.Getpid(), // this test process is certainly alive
	}
	dead := instance.Entry{
		Name:       "ghost",
		Kind:       instance.KindCartridge,
		StateDir:   filepath.Join(root, "mnt", "ghost"),
		Mountpoint: filepath.Join(root, "mnt", "ghost"),
		DevNode:    "/dev/disk12",
		SourcePath: filepath.Join(root, "Downloads", "ghost.dmg"),
	}
	for _, e := range []instance.Entry{live, dead} {
		if err := instance.Write(root, e); err != nil {
			t.Fatalf("write registry entry %q: %v", e.Name, err)
		}
	}

	held := heldVolumes(root)
	tests := []struct {
		name     string
		volume   heldVolume
		wantName string
		wantHeld bool
	}{
		{name: "by mountpoint", volume: heldVolume{Mountpoint: mount}, wantName: "demo", wantHeld: true},
		{name: "by the slice of the held whole disk", volume: heldVolume{DevNode: "/dev/disk9s1"}, wantName: "demo", wantHeld: true},
		{name: "by bare bsd name", volume: heldVolume{DevNode: "disk9s1s2"}, wantName: "demo", wantHeld: true},
		{
			// A SECOND Finder mount of the booted .dmg: macOS gives it its own
			// volume path and its own device, so only the source connects them.
			name: "by the source image of a second, independent mount",
			volume: heldVolume{
				Mountpoint: "/Volumes/bladerunner-demo 1",
				DevNode:    "/dev/disk12s1",
				SourcePath: source,
			},
			wantName: "demo", wantHeld: true,
		},
		{
			name:     "by the working copy the holder converted",
			volume:   heldVolume{Mountpoint: "/Volumes/bladerunner-demo 1", SourcePath: working},
			wantName: "demo", wantHeld: true,
		},
		{
			name:   "a different disk is not held",
			volume: heldVolume{Mountpoint: "/Volumes/bladerunner-other", DevNode: "/dev/disk4s1", SourcePath: filepath.Join(root, "Downloads", "other.dmg")},
		},
		{
			name:   "a dead holder does not hold anything",
			volume: heldVolume{Mountpoint: dead.Mountpoint, DevNode: dead.DevNode, SourcePath: dead.SourcePath},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, ok := held(tt.volume)
			if ok != tt.wantHeld || name != tt.wantName {
				t.Errorf("held(%+v) = (%q, %v), want (%q, %v)",
					tt.volume, name, ok, tt.wantName, tt.wantHeld)
			}
		})
	}
}

// The accept path's regression: a second mount of a cartridge that is already
// booted must be recognized as held even though its mountpoint and device are
// brand new. Matching only on those two — as the watcher did — offered the
// cartridge again, and accepting the offer booted the same image twice.
func TestDecideForVolumeIgnoresASecondMountOfABootedCartridge(t *testing.T) {
	source := "/Users/someone/Downloads/demo.dmg"
	// The holder's own mount is /Volumes/bladerunner-demo on disk9; this is the
	// SECOND mount macOS made of the same file.
	second := diskarb.DiskInfo{
		BSDName:    "disk12s1",
		VolumeName: "bladerunner-demo 1",
		VolumePath: "/Volumes/bladerunner-demo 1",
		VolumeKind: "apfs",
	}
	detected := bootableDetected("demo")
	detected.Mountpoint = second.VolumePath
	detected.DevNode = "/dev/disk12s1"
	detected.BackingImage = source

	// A holder that recorded only what it itself attached: another mountpoint,
	// another device, the same image.
	held := func(v heldVolume) (string, bool) {
		if v.SourcePath != "" && cartridgeImageKey(v.SourcePath) == cartridgeImageKey(source) {
			return "demo", true
		}
		return "", false
	}

	got := decideForVolume(second, detectReturning(detected, nil, nil), held)
	if got.Verdict != verdictIgnore || got.HeldBy != "demo" {
		t.Fatalf("verdict = %q heldBy = %q, want ignore/demo (booting it again would run one image twice)",
			got.Verdict, got.HeldBy)
	}
}

// recordingSink collects the actions a watcher delivers.
type recordingSink struct {
	mu      sync.Mutex
	actions []watchAction
}

func (s *recordingSink) handle(a watchAction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = append(s.actions, a)
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.actions)
}

// testWatcher builds a watcher over a fixed Detect answer and nothing held. A
// nil answer stands for "not one of ours".
func testWatcher(d *cartridge.Detected, sink *recordingSink, calls *int) *cartridgeWatcher {
	if d == nil {
		d = &cartridge.Detected{Status: cartridge.StatusNotCartridge}
	}
	return &cartridgeWatcher{
		detect: detectReturning(d, nil, calls),
		held:   func() heldFunc { return nil },
		sink:   sink.handle,
		seen:   make(map[string]bool),
	}
}

func TestWatcherOffersOncePerVolume(t *testing.T) {
	const mount = testCartridgeMount
	disk := diskarb.DiskInfo{BSDName: "disk9s1", VolumeName: "bladerunner-demo", VolumePath: mount}
	sink := &recordingSink{}
	w := testWatcher(bootableDetected("demo"), sink, nil)

	w.observe(disk)
	w.observe(disk)
	// The same mount reported for the whole disk collapses onto the same key.
	w.observe(diskarb.DiskInfo{BSDName: "disk9", VolumeName: "bladerunner-demo", VolumePath: mount})
	if got := sink.count(); got != 1 {
		t.Fatalf("sink called %d times, want 1 (the user must not be re-prompted)", got)
	}

	// Ejecting and re-inserting is a new question.
	w.forget(disk)
	w.observe(disk)
	if got := sink.count(); got != 2 {
		t.Fatalf("sink called %d times after re-insertion, want 2", got)
	}
}

func TestWatcherCatchUpMergesWithTheAppearedStream(t *testing.T) {
	const mount = testCartridgeMount
	disk := diskarb.DiskInfo{BSDName: "disk9s1", VolumeName: "bladerunner-demo", VolumePath: mount}
	unrelated := diskarb.DiskInfo{BSDName: "disk1s1", VolumeName: "Macintosh HD", VolumePath: "/"}

	sink := &recordingSink{}
	detects := 0
	w := testWatcher(bootableDetected("demo"), sink, &detects)

	// A cartridge mounted before the watch started, then reported again by the
	// appeared stream: exactly one offer.
	w.catchUp([]diskarb.DiskInfo{unrelated, disk})
	w.observe(disk)

	if got := sink.count(); got != 1 {
		t.Fatalf("sink called %d times, want 1", got)
	}
	if detects != 1 {
		t.Fatalf("Detect called %d times, want 1 (unrelated volumes must not be read)", detects)
	}
	if a := sink.actions[0]; a.Verdict != verdictOffer || a.Name != "demo" {
		t.Fatalf("action = %+v, want an offer for demo", a)
	}
}

// An ignored volume must not be remembered: a menubar session sees every volume
// on the machine, and remembering them all would grow without bound.
func TestWatcherDoesNotRememberIgnoredVolumes(t *testing.T) {
	sink := &recordingSink{}
	w := testWatcher(nil, sink, nil)
	w.observe(diskarb.DiskInfo{BSDName: "disk3s1", VolumeName: "Backup", VolumePath: "/Volumes/Backup"})
	if len(w.seen) != 0 {
		t.Fatalf("seen = %v, want empty", w.seen)
	}
	if got := sink.count(); got != 0 {
		t.Fatalf("sink called %d times for an unrelated volume, want 0", got)
	}
}

// The BSD-name reductions themselves are specified once, in
// internal/diskarb (TestBSDNameRuleOverEverySpelling). What this file still owns
// is that the watcher applies them where it must: the key it dedupes volumes on
// is the whole-disk unit, so the same mount reported for the whole disk and for
// its slice is one volume.
func TestVolumeKey(t *testing.T) {
	tests := []struct {
		name string
		disk diskarb.DiskInfo
		want string
	}{
		{"prefers the whole-disk unit", diskarb.DiskInfo{BSDName: "disk9s1", VolumePath: "/Volumes/x"}, "disk9"},
		{"pairs a slice with its whole disk", diskarb.DiskInfo{BSDName: "disk9", VolumePath: "/Volumes/x"}, "disk9"},
		{"falls back to the mountpoint", diskarb.DiskInfo{VolumePath: "/Volumes/x/"}, "/Volumes/x"},
		{"falls back to the volume name", diskarb.DiskInfo{VolumeName: "x"}, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := volumeKey(tt.disk); got != tt.want {
				t.Errorf("volumeKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A decided volume carries the device in the /dev form the instance registry
// records, whatever spelling DiskArbitration reported it under — that is what
// makes the held-by-device lookup line up.
func TestDecidedVolumeCarriesTheDevicePath(t *testing.T) {
	got := decideForVolume(diskarb.DiskInfo{BSDName: "disk9s1"}, nil, nil)
	if got.DevNode != "/dev/disk9s1" {
		t.Errorf("DevNode = %q, want /dev/disk9s1", got.DevNode)
	}
	if unnamed := decideForVolume(diskarb.DiskInfo{}, nil, nil); unnamed.DevNode != "" {
		t.Errorf("DevNode for a disk with no BSD name = %q, want empty", unnamed.DevNode)
	}
}

// bootDetectedCartridge must refuse an offer that carries no source file rather
// than fall back to booting the read-only mounted view.
func TestBootDetectedCartridgeRequiresASource(t *testing.T) {
	_, err := bootDetectedCartridge(watchAction{Verdict: verdictOffer, Name: "demo"})
	if !errors.Is(err, errNoCartridgeSource) {
		t.Fatalf("error = %v, want errNoCartridgeSource", err)
	}
}

// A smoke test over the real DiskArbitration wiring: no DMG, no cartridge, just
// "the session opens, the watchers register and it all tears down".
func TestStartCartridgeWatchAgainstRealDiskArbitration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("DiskArbitration is macOS-only")
	}
	if testing.Short() {
		t.Skip("short mode: skipping the real DiskArbitration session")
	}
	sink := &recordingSink{}
	w := testWatcher(nil, sink, nil)
	stop, err := startCartridgeWatch(w)
	if err != nil {
		t.Fatalf("startCartridgeWatch() error = %v", err)
	}
	stop()
	stop() // idempotent: a second teardown must not panic
}
