//go:build darwin

package vmhost

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/instance"
)

// Every bail-out in startUnmountWatch disables the unmount veto and returns
// nil, logging at Warn. That is the deliberate fail-open behavior AND the
// exact shape of a bug that already shipped: protection silently absent, VM
// running, nobody any the wiser. These tests pin both halves — the start is
// never failed, and which bail-out fired is recorded.

// errNoSession stands in for whatever DiskArbitration failed with.
var errNoSession = errors.New("no DiskArbitration for you")

// fakeUnmountSession is an injected diskarb session: it records what filter was
// registered, can refuse to register at all, and counts its own Close so a
// failed registration can be shown not to leak the session.
type fakeUnmountSession struct {
	watchErr   error
	filter     string
	registered atomic.Int64
	canceled   atomic.Int64
	closed     atomic.Int64
}

func (f *fakeUnmountSession) WatchUnmountApproval(bsdName string, _ func(diskarb.DiskInfo) diskarb.Dissent) (diskarb.CancelFunc, error) {
	f.filter = bsdName
	if f.watchErr != nil {
		return nil, f.watchErr
	}
	f.registered.Add(1)
	return func() { f.canceled.Add(1) }, nil
}

func (f *fakeUnmountSession) Close() error {
	f.closed.Add(1)
	return nil
}

// cartridgeHost builds a cartridge Host whose mount records devNode, with the
// session constructor replaced by newSession.
func cartridgeHost(t *testing.T, devNode string, newSession func() (unmountSession, error)) *Host {
	t.Helper()
	host, err := New(Spec{
		Kind:          instance.KindCartridge,
		Name:          "veto-test",
		CartridgePath: "/tmp/demo.dmg",
		Mountpoint:    "/Volumes/bladerunner-demo",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host.AdoptCartridge(&cartridge.Opened{
		Name:  "demo",
		Mount: cartridge.Mount{Mountpoint: "/Volumes/bladerunner-demo", DevNode: devNode},
	})
	host.newUnmountSession = newSession
	return host
}

// The four ways protection is lost, plus the one that is not a loss at all.
// Each must leave the start unfailed, arm no watcher, and say which it was.
func TestStartUnmountWatchBailOutsAreRecorded(t *testing.T) {
	failingSession := func() (unmountSession, error) { return nil, errNoSession }

	cases := []struct {
		name string
		host func(t *testing.T) *Host
		want UnprotectedReason
	}{
		{
			name: "the cartridge step produced no mount",
			host: func(t *testing.T) *Host {
				t.Helper()
				host, err := New(Spec{Kind: instance.KindCartridge, Name: "veto-test", CartridgePath: "/tmp/demo.dmg", Mountpoint: "/Volumes/bladerunner-demo"})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return host
			},
			want: UnprotectedNoCartridge,
		},
		{
			name: "the mount recorded no device node",
			host: func(t *testing.T) *Host { return cartridgeHost(t, "", nil) },
			want: UnprotectedNoDevNode,
		},
		{
			name: "the recorded device node names no BSD disk",
			host: func(t *testing.T) *Host { return cartridgeHost(t, "/Volumes/bladerunner-demo", nil) },
			want: UnprotectedUnreadableDevNode,
		},
		{
			name: "DiskArbitration would not open",
			host: func(t *testing.T) *Host { return cartridgeHost(t, "/dev/disk9s1", failingSession) },
			want: UnprotectedNoSession,
		},
		{
			name: "the approval watcher would not register",
			host: func(t *testing.T) *Host {
				t.Helper()
				return cartridgeHost(t, "/dev/disk9s1", func() (unmountSession, error) {
					return &fakeUnmountSession{watchErr: errors.New("register refused")}, nil
				})
			},
			want: UnprotectedWatchFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := tc.host(t)
			if err := host.startUnmountWatch(); err != nil {
				t.Fatalf("startUnmountWatch failed the start: %v — the veto must fail open", err)
			}
			if got := host.UnmountProtection(); got != tc.want {
				t.Errorf("UnmountProtection() = %q, want %q", got, tc.want)
			}
			if host.unmountCancel != nil {
				t.Error("a bail-out must leave no watcher registered")
			}
			// Nothing was armed, so teardown has nothing to undo.
			if err := host.stopUnmountWatch(); err != nil {
				t.Errorf("stopUnmountWatch: %v", err)
			}
		})
	}
}

// A session that opened but could not register the watcher must be closed:
// the veto is off either way, and leaking the session would leak its dispatch
// queue for the life of the holder.
func TestStartUnmountWatchClosesTheSessionWhenRegistrationFails(t *testing.T) {
	session := &fakeUnmountSession{watchErr: errors.New("register refused")}
	host := cartridgeHost(t, "/dev/disk9s1", func() (unmountSession, error) { return session, nil })

	if err := host.startUnmountWatch(); err != nil {
		t.Fatalf("startUnmountWatch: %v", err)
	}
	if got := session.closed.Load(); got != 1 {
		t.Fatalf("session closed %d times after a failed registration, want 1", got)
	}
}

// The success path: the watcher is armed with the BARE BSD name reduced from
// whatever spelling the mount recorded, protection reports armed, and teardown
// both cancels the watcher and closes the session.
func TestStartUnmountWatchArmsTheVeto(t *testing.T) {
	session := &fakeUnmountSession{}
	host := cartridgeHost(t, "/dev/disk9s1", func() (unmountSession, error) { return session, nil })

	if err := host.startUnmountWatch(); err != nil {
		t.Fatalf("startUnmountWatch: %v", err)
	}
	if got := host.UnmountProtection(); got != UnprotectedNone {
		t.Fatalf("UnmountProtection() = %q, want armed (%q)", got, UnprotectedNone)
	}
	if host.unmountCancel == nil {
		t.Fatal("no watcher was registered")
	}
	if session.filter != "disk9s1" {
		t.Errorf("registered filter = %q, want the bare BSD name %q", session.filter, "disk9s1")
	}
	if !diskarb.MatchesFilter(session.filter, "disk9s1") {
		t.Errorf("filter %q does not match the name DiskArbitration reports", session.filter)
	}

	if err := host.stopUnmountWatch(); err != nil {
		t.Fatalf("stopUnmountWatch: %v", err)
	}
	if got := session.canceled.Load(); got != 1 {
		t.Errorf("watcher canceled %d times, want 1", got)
	}
	if got := session.closed.Load(); got != 1 {
		t.Errorf("session closed %d times, want 1", got)
	}
}

// A non-cartridge instance has nothing to protect, so it is not "unprotected":
// reporting a reason there would cry wolf on every flat instance.
func TestStartUnmountWatchLeavesFlatInstancesProtectionless(t *testing.T) {
	host := newTestHost(t)
	if err := host.startUnmountWatch(); err != nil {
		t.Fatalf("startUnmountWatch: %v", err)
	}
	if got := host.UnmountProtection(); got != UnprotectedNone {
		t.Fatalf("UnmountProtection() for a flat instance = %q, want %q", got, UnprotectedNone)
	}
}

// Without an injected constructor the veto reaches for a real DiskArbitration
// session, which is what a holder does. Whether the framework is available in
// the test environment is not the assertion: the assertion is that either
// outcome leaves the Host consistent — armed, or unprotected with a stated
// reason — and never fails the start.
func TestStartUnmountWatchUsesRealDiskArbitrationByDefault(t *testing.T) {
	host := cartridgeHost(t, "/dev/disk9s1", nil)
	if err := host.startUnmountWatch(); err != nil {
		t.Fatalf("startUnmountWatch: %v", err)
	}
	t.Cleanup(func() {
		if err := host.stopUnmountWatch(); err != nil {
			t.Errorf("stopUnmountWatch: %v", err)
		}
	})
	switch got := host.UnmountProtection(); got {
	case UnprotectedNone:
		if host.unmountCancel == nil {
			t.Fatal("protection reports armed but no watcher is registered")
		}
	case UnprotectedNoSession, UnprotectedWatchFailed:
		if host.unmountCancel != nil {
			t.Fatal("protection reports off but a watcher is registered")
		}
	default:
		t.Fatalf("UnmountProtection() = %q, want armed or a registration failure", got)
	}
}
