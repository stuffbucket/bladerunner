package diskarb

import (
	"errors"
	"runtime"
	"testing"
)

func TestWholeDiskName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "whole disk unchanged", in: "disk4", want: "disk4"},
		{name: "slice reduced", in: "disk4s1", want: "disk4"},
		{name: "nested slice reduced", in: "disk4s1s2", want: "disk4"},
		{name: "multi digit unit", in: "disk12s3", want: "disk12"},
		{name: "different unit kept apart", in: "disk40s1", want: "disk40"},
		{name: "empty", in: "", want: ""},
		{name: "prefix only", in: "disk", want: "disk"},
		{name: "not a disk device", in: "cdrom0", want: "cdrom0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := wholeDiskName(tt.in); got != tt.want {
				t.Errorf("wholeDiskName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBSDNameMatches(t *testing.T) {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := bsdNameMatches(tt.want, tt.got); got != tt.ok {
				t.Errorf("bsdNameMatches(%q, %q) = %v, want %v", tt.want, tt.got, got, tt.ok)
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
