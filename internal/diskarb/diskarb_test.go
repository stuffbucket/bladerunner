package diskarb

import (
	"errors"
	"fmt"
	"runtime"
	"testing"
)

func TestMatchesFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want string
		got  string
		ok   bool
	}{
		{name: "empty filter matches anything", want: "", got: "disk4s1", ok: true},
		{name: "empty filter matches unnamed disk", want: "", got: "", ok: true},
		{name: "exact match", want: "disk4s1", got: "disk4s1", ok: true},
		{name: "whole disk matches slice", want: "disk4", got: "disk4s1", ok: true},
		{name: "slice matches whole disk", want: "disk4s1", got: "disk4", ok: true},
		{name: "sibling slices match", want: "disk4s1", got: "disk4s2", ok: true},
		{name: "different unit does not match", want: "disk4", got: "disk5s1", ok: false},
		{name: "prefix collision does not match", want: "disk4", got: "disk40s1", ok: false},
		{name: "unnamed disk does not match a filter", want: "disk4", got: "", ok: false},
		// The regression: a filter registered with the recorded device PATH has
		// to match the bare name DiskArbitration reports, or the unmount veto
		// never fires.
		{name: "device path filter matches the reported bare name", want: "/dev/disk4s1", got: "disk4s1", ok: true},
		{name: "whole disk path filter matches the slice", want: "/dev/disk4", got: "disk4s1", ok: true},
		{name: "raw device path filter matches the slice", want: "/dev/rdisk4", got: "disk4s1", ok: true},
		{name: "device path filter still keeps units apart", want: "/dev/disk4", got: "disk5s1", ok: false},
		// A filter that names no device at all must not degenerate into
		// "match everything": only "" means that.
		{name: "non device filter does not match", want: "/Volumes/demo", got: "disk4s1", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := MatchesFilter(tt.want, tt.got); got != tt.ok {
				t.Errorf("MatchesFilter(%q, %q) = %v, want %v", tt.want, tt.got, got, tt.ok)
			}
		})
	}
}

func TestDissentHelpers(t *testing.T) {
	t.Parallel()

	if d := Approve(); d.Deny {
		t.Errorf("Approve() = %+v, want Deny false", d)
	}
	if d := (Dissent{}); d.Deny {
		t.Errorf("zero Dissent = %+v, want Deny false", d)
	}

	const reason = "draining the VM"
	d := Deny(reason)
	if !d.Deny {
		t.Errorf("Deny(%q).Deny = false, want true", reason)
	}
	if d.Reason != reason {
		t.Errorf("Deny(%q).Reason = %q, want %q", reason, d.Reason, reason)
	}
}

func TestDiskInfoMounted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info DiskInfo
		want bool
	}{
		{name: "mounted volume", info: DiskInfo{VolumePath: "/Volumes/cartridge"}, want: true},
		{name: "bare media", info: DiskInfo{BSDName: "disk4"}, want: false},
		{name: "zero value", info: DiskInfo{}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.info.Mounted(); got != tt.want {
				t.Errorf("Mounted() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestErrUnsupportedWrapsStdlib(t *testing.T) {
	t.Parallel()

	if !errors.Is(ErrUnsupported, errors.ErrUnsupported) {
		t.Errorf("ErrUnsupported does not wrap errors.ErrUnsupported")
	}
}

// TestStubsOffDarwin pins the non-darwin contract: every entry point reports
// ErrUnsupported rather than pretending to work. It is a no-op on darwin, where
// diskarb_darwin_test.go exercises the real implementation.
func TestStubsOffDarwin(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "darwin" {
		t.Skip("darwin has a real DiskArbitration implementation")
	}

	if _, err := NewSession(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("NewSession() error = %v, want ErrUnsupported", err)
	}
	if _, err := CurrentDisks(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CurrentDisks() error = %v, want ErrUnsupported", err)
	}

	var s Session
	if _, err := s.WatchAppeared(func(DiskInfo) {}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("WatchAppeared() error = %v, want ErrUnsupported", err)
	}
	if _, err := s.WatchDisappeared(func(DiskInfo) {}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("WatchDisappeared() error = %v, want ErrUnsupported", err)
	}
	if _, err := s.WatchUnmountApproval("", func(DiskInfo) Dissent { return Approve() }); !errors.Is(err, ErrUnsupported) {
		t.Errorf("WatchUnmountApproval() error = %v, want ErrUnsupported", err)
	}
	if _, err := s.CurrentDisks(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CurrentDisks() error = %v, want ErrUnsupported", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func TestDiskInfoTrackingKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info DiskInfo
		want string
	}{
		{
			name: "bsd name preferred",
			info: DiskInfo{BSDName: "disk9s1", VolumePath: "/Volumes/cartridge"},
			want: "disk9s1",
		},
		{
			name: "bsd name survives an unmount",
			info: DiskInfo{BSDName: "disk9s1"},
			want: "disk9s1",
		},
		{
			name: "mount point is the fallback",
			info: DiskInfo{VolumePath: "/Volumes/snapshot"},
			want: "/Volumes/snapshot",
		},
		{name: "nothing to key on", info: DiskInfo{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.info.trackingKey(); got != tt.want {
				t.Errorf("trackingKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestVolumeTrackerMountLifecycle is the unit-level half of the regression test
// for the appeared/changed ordering bug. DiskArbitration announces the media
// before diskarbitrationd mounts it, so the sequence a real insertion produces
// is "appeared with no volume path" followed by "description changed, volume
// path present" — and the mount must be reported once, on the second event.
func TestVolumeTrackerMountLifecycle(t *testing.T) {
	t.Parallel()

	const (
		bsd  = "disk9s1"
		path = "/Volumes/cartridge"
	)
	bare := DiskInfo{BSDName: bsd, VolumeName: "cartridge"}
	mounted := DiskInfo{BSDName: bsd, VolumeName: "cartridge", VolumePath: path}

	tr := newVolumeTracker()

	// Media appeared, nothing mounted on it yet.
	if state, _ := tr.observe(bare, false); state != mountUnchanged {
		t.Errorf("bare media: state = %v, want mountUnchanged", state)
	}
	// diskarbitrationd mounted it: this is the event callers care about.
	state, info := tr.observe(mounted, true)
	if state != mountAppeared {
		t.Fatalf("mount: state = %v, want mountAppeared", state)
	}
	if info.VolumePath != path {
		t.Errorf("mount: VolumePath = %q, want %q", info.VolumePath, path)
	}
	// Any further description change must not re-announce it.
	if state, _ := tr.observe(mounted, true); state != mountUnchanged {
		t.Errorf("redundant change: state = %v, want mountUnchanged", state)
	}
	// Unmounted: the description no longer carries a path, but the caller is
	// told where the volume used to be.
	state, info = tr.observe(bare, false)
	if state != mountVanished {
		t.Fatalf("unmount: state = %v, want mountVanished", state)
	}
	if info.VolumePath != path {
		t.Errorf("unmount: VolumePath = %q, want the remembered %q", info.VolumePath, path)
	}
	// The media disappearing afterwards must not report a second vanish.
	if state, _ := tr.observe(bare, false); state != mountUnchanged {
		t.Errorf("media gone after unmount: state = %v, want mountUnchanged", state)
	}
	// Re-inserting the same cartridge must be announced again: forgetting the
	// unmount is what would otherwise debounce it forever.
	if state, _ := tr.observe(mounted, true); state != mountAppeared {
		t.Errorf("re-insert: state = %v, want mountAppeared", state)
	}
}

// TestVolumeTrackerReplayThenChange covers the other order: the registration
// replay already carries a volume path, so the description change that follows
// must not announce the same volume a second time.
func TestVolumeTrackerReplayThenChange(t *testing.T) {
	t.Parallel()

	mounted := DiskInfo{BSDName: "disk4s1", VolumePath: "/Volumes/replayed"}
	tr := newVolumeTracker()

	if state, _ := tr.observe(mounted, true); state != mountAppeared {
		t.Fatalf("replay: state != mountAppeared")
	}
	for i := range 3 {
		if state, _ := tr.observe(mounted, true); state != mountUnchanged {
			t.Errorf("change %d: state = %v, want mountUnchanged", i, state)
		}
	}

	// Renaming a volume moves its mount point without unmounting it. The
	// unmount that follows must report where the volume actually was, not where
	// it was first seen.
	renamed := DiskInfo{BSDName: mounted.BSDName, VolumePath: "/Volumes/renamed"}
	if state, _ := tr.observe(renamed, true); state != mountUnchanged {
		t.Errorf("rename: state = %v, want mountUnchanged", state)
	}
	state, info := tr.observe(DiskInfo{BSDName: mounted.BSDName}, false)
	if state != mountVanished {
		t.Fatalf("unmount: state = %v, want mountVanished", state)
	}
	if info.VolumePath != renamed.VolumePath {
		t.Errorf("unmount: VolumePath = %q, want %q", info.VolumePath, renamed.VolumePath)
	}
}

// TestVolumeTrackerIgnoresUnknownUnmount pins that a volume the tracker never
// saw mounted cannot produce a vanish event, which is what keeps every bare
// device and APFS container on the machine out of the callers' way.
func TestVolumeTrackerIgnoresUnknownUnmount(t *testing.T) {
	t.Parallel()

	tr := newVolumeTracker()
	if state, _ := tr.observe(DiskInfo{BSDName: "disk7"}, false); state != mountUnchanged {
		t.Errorf("state = %v, want mountUnchanged", state)
	}
	if state, _ := tr.observe(DiskInfo{}, true); state != mountUnchanged {
		t.Errorf("keyless disk: state = %v, want mountUnchanged", state)
	}
}

// TestVolumeTrackerIsBounded pins the memory bound: past maxTrackedVolumes the
// tracker keeps announcing mounts but stops remembering them, so a runaway
// event stream cannot grow the map without limit.
func TestVolumeTrackerIsBounded(t *testing.T) {
	t.Parallel()

	tr := newVolumeTracker()
	for i := range maxTrackedVolumes * 2 {
		info := DiskInfo{BSDName: fmt.Sprintf("disk%d", i), VolumePath: fmt.Sprintf("/Volumes/v%d", i)}
		if state, _ := tr.observe(info, true); state != mountAppeared {
			t.Fatalf("volume %d: state = %v, want mountAppeared", i, state)
		}
	}
	if len(tr.mounted) > maxTrackedVolumes {
		t.Errorf("tracker remembers %d volumes, want at most %d", len(tr.mounted), maxTrackedVolumes)
	}
}

// TestVolumeTrackerForgetIsTerminal makes sure a late event delivered after the
// owning watcher was canceled cannot write into the released map.
func TestVolumeTrackerForgetIsTerminal(t *testing.T) {
	t.Parallel()

	tr := newVolumeTracker()
	mounted := DiskInfo{BSDName: "disk9s1", VolumePath: "/Volumes/cartridge"}
	if state, _ := tr.observe(mounted, true); state != mountAppeared {
		t.Fatalf("state != mountAppeared")
	}
	tr.forget()
	if state, _ := tr.observe(mounted, true); state != mountUnchanged {
		t.Errorf("after forget: state = %v, want mountUnchanged", state)
	}
	if state, _ := tr.observe(mounted, false); state != mountUnchanged {
		t.Errorf("after forget: state = %v, want mountUnchanged", state)
	}
}
