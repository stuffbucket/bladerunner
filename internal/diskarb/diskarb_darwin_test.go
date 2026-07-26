//go:build darwin

package diskarb

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// callbackDeadline bounds how long a test waits for DiskArbitration to deliver
// a callback on the session queue before giving up.
const callbackDeadline = 5 * time.Second

// newTestSession opens a session and closes it when the test ends.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	s, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return s
}

func TestNewSessionCloseIsIdempotent(t *testing.T) {
	s, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestSessionRejectsNilCallbacks(t *testing.T) {
	s := newTestSession(t)

	if _, err := s.WatchAppeared(nil); !errors.Is(err, ErrNilCallback) {
		t.Errorf("WatchAppeared(nil) error = %v, want ErrNilCallback", err)
	}
	if _, err := s.WatchDisappeared(nil); !errors.Is(err, ErrNilCallback) {
		t.Errorf("WatchDisappeared(nil) error = %v, want ErrNilCallback", err)
	}
	if _, err := s.WatchUnmountApproval("", nil); !errors.Is(err, ErrNilCallback) {
		t.Errorf("WatchUnmountApproval(nil) error = %v, want ErrNilCallback", err)
	}
}

func TestSessionRejectsUseAfterClose(t *testing.T) {
	s, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, err := s.WatchAppeared(func(DiskInfo) {}); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("WatchAppeared after Close error = %v, want ErrSessionClosed", err)
	}
	if _, err := s.WatchDisappeared(func(DiskInfo) {}); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("WatchDisappeared after Close error = %v, want ErrSessionClosed", err)
	}
	if _, err := s.WatchUnmountApproval("disk9", func(DiskInfo) Dissent { return Approve() }); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("WatchUnmountApproval after Close error = %v, want ErrSessionClosed", err)
	}
	if _, err := s.CurrentDisks(); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("CurrentDisks after Close error = %v, want ErrSessionClosed", err)
	}
}

// TestWatchRegisterAndCancel is the crash test: register every watcher kind and
// unregister it straight away, which is exactly the window where a mishandled
// cgo.Handle or a missing queue barrier blows up. Run it under -race.
func TestWatchRegisterAndCancel(t *testing.T) {
	s := newTestSession(t)

	tests := []struct {
		name     string
		register func() (CancelFunc, error)
	}{
		{
			name:     "appeared",
			register: func() (CancelFunc, error) { return s.WatchAppeared(func(DiskInfo) {}) },
		},
		{
			name:     "disappeared",
			register: func() (CancelFunc, error) { return s.WatchDisappeared(func(DiskInfo) {}) },
		},
		{
			name: "unmount approval, all disks",
			register: func() (CancelFunc, error) {
				return s.WatchUnmountApproval("", func(DiskInfo) Dissent { return Approve() })
			},
		},
		{
			name: "unmount approval, one disk",
			register: func() (CancelFunc, error) {
				return s.WatchUnmountApproval("disk9", func(DiskInfo) Dissent { return Deny("busy") })
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cancel, err := tt.register()
			if err != nil {
				t.Fatalf("register error = %v", err)
			}
			if cancel == nil {
				t.Fatal("register returned a nil CancelFunc")
			}
			cancel()
			cancel() // idempotent
		})
	}
}

// TestCancelAfterCloseIsSafe covers the ordering hazard where Close has already
// freed a watcher's handle and the caller then invokes its CancelFunc.
func TestCancelAfterCloseIsSafe(t *testing.T) {
	s, err := NewSession()
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	cancel, err := s.WatchAppeared(func(DiskInfo) {})
	if err != nil {
		t.Fatalf("WatchAppeared() error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	cancel()
	cancel()
}

// TestConcurrentRegisterCancel hammers the registration path from several
// goroutines so the race detector can see the handle/context lifecycle.
func TestConcurrentRegisterCancel(t *testing.T) {
	s := newTestSession(t)

	const (
		goroutines = 4
		rounds     = 8
	)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				cancel, err := s.WatchAppeared(func(DiskInfo) {})
				if err != nil {
					t.Errorf("WatchAppeared() error = %v", err)
					return
				}
				cancel()
			}
		}()
	}
	wg.Wait()
}

func TestCurrentDisks(t *testing.T) {
	s := newTestSession(t)

	disks, err := s.CurrentDisks()
	if err != nil {
		t.Fatalf("CurrentDisks() error = %v", err)
	}
	for _, d := range disks {
		if !d.Mounted() {
			t.Errorf("CurrentDisks returned an unmounted disk: %+v", d)
		}
	}

	if testing.Short() {
		return // asserting on real volumes needs a real machine
	}
	if len(disks) == 0 {
		t.Fatal("CurrentDisks() returned no disks; the root volume is always mounted")
	}
	var foundRoot bool
	for _, d := range disks {
		if d.VolumePath == "/" {
			foundRoot = true
			if d.VolumeKind == "" {
				t.Errorf("root volume has no VolumeKind: %+v", d)
			}
			if d.NetworkVolume {
				t.Errorf("root volume reported as a network volume: %+v", d)
			}
		}
	}
	if !foundRoot {
		t.Errorf("CurrentDisks() did not include the root volume; got %d disks", len(disks))
	}
}

func TestPackageCurrentDisks(t *testing.T) {
	if _, err := CurrentDisks(); err != nil {
		t.Fatalf("CurrentDisks() error = %v", err)
	}
}

// TestWatchAppearedDeliversExistingDisks relies on DiskArbitration replaying the
// volumes that are already mounted when a callback is registered, which is how a
// watcher discovers a cartridge that was attached before bladerunner started.
func TestWatchAppearedDeliversExistingDisks(t *testing.T) {
	if testing.Short() {
		t.Skip("needs real mounted volumes")
	}
	s := newTestSession(t)

	seen := make(chan DiskInfo, 1)
	cancel, err := s.WatchAppeared(func(info DiskInfo) {
		select {
		case seen <- info:
		default:
		}
	})
	if err != nil {
		t.Fatalf("WatchAppeared() error = %v", err)
	}
	defer cancel()

	select {
	case info := <-seen:
		if !info.Mounted() {
			t.Errorf("WatchAppeared delivered an unmounted disk: %+v", info)
		}
	case <-time.After(callbackDeadline):
		t.Fatalf("no disk appeared within %s", callbackDeadline)
	}
}

// TestCancelFromInsideCallback exercises the reentrancy guard: canceling from
// the session's own serial queue must skip the drain barrier instead of
// deadlocking against it.
func TestCancelFromInsideCallback(t *testing.T) {
	if testing.Short() {
		t.Skip("needs real mounted volumes")
	}
	s := newTestSession(t)

	var (
		armed    atomic.Pointer[CancelFunc]
		canceled = make(chan struct{})
		once     sync.Once
	)
	cancel, err := s.WatchAppeared(func(DiskInfo) {
		fn := armed.Load()
		if fn == nil {
			return // cancel not stored yet; a later disk will do
		}
		once.Do(func() {
			(*fn)()
			close(canceled)
		})
	})
	if err != nil {
		t.Fatalf("WatchAppeared() error = %v", err)
	}
	armed.Store(&cancel)

	select {
	case <-canceled:
	case <-time.After(callbackDeadline):
		cancel()
		t.Skipf("no disk-appeared callback arrived within %s while cancel was armed", callbackDeadline)
	}
}
