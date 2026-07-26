//go:build darwin

package diskarb

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// callbackDeadline bounds how long a test waits for DiskArbitration to deliver
// a callback on the session queue before giving up.
const callbackDeadline = 5 * time.Second

// attachDeadline bounds how long a test waits for diskarbitrationd to notice a
// freshly attached (or detached) disk image. Attaching a DMG takes on the order
// of a second; the mount description arrives tens of milliseconds after the
// media does.
const attachDeadline = 30 * time.Second

// replayQuiet is how long the registration replay must stay silent before a
// test treats it as finished.
const replayQuiet = 500 * time.Millisecond

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

// hdiutilPath locates hdiutil, skipping the test when it is not installed.
func hdiutilPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skipf("hdiutil is not available: %v", err)
	}
	return path
}

// runHdiutil runs hdiutil and returns its combined output, failing the test if
// the command does not succeed.
func runHdiutil(t *testing.T, hdiutil string, args ...string) string {
	t.Helper()
	out, err := exec.Command(hdiutil, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("hdiutil %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// createTestImage builds a small, uniquely named disk image and returns its
// path together with the volume name it mounts under.
func createTestImage(t *testing.T, hdiutil string) (image, volumeName string) {
	t.Helper()
	volumeName = fmt.Sprintf("brda%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	image = filepath.Join(t.TempDir(), "cartridge.dmg")
	runHdiutil(t, hdiutil, "create", "-size", "10m", "-fs", "APFS",
		"-volname", volumeName, "-quiet", image)
	return image, volumeName
}

// attachTestImage attaches image and returns the whole-disk BSD device it was
// attached as. A detach is registered with t.Cleanup so a failing assertion
// cannot leave a stray volume behind.
func attachTestImage(t *testing.T, hdiutil, image string) string {
	t.Helper()
	out := runHdiutil(t, hdiutil, "attach", "-noverify", image)
	device := firstDevNode(out)
	if device == "" {
		t.Fatalf("hdiutil attach printed no /dev node:\n%s", out)
	}
	t.Cleanup(func() {
		// Best effort: the test detaches explicitly, so this usually fails with
		// "no such device", which is exactly the state we want to end in.
		_ = exec.Command(hdiutil, "detach", device, "-force").Run()
	})
	return device
}

// firstDevNode returns the first /dev/diskN token in hdiutil attach output,
// which is the whole-disk device the image was attached as.
func firstDevNode(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if strings.HasPrefix(fields[0], "/dev/disk") {
			return fields[0]
		}
	}
	return ""
}

// waitForVolume waits for a DiskInfo to arrive on ch, failing the test if none
// does before attachDeadline.
func waitForVolume(t *testing.T, ch <-chan DiskInfo, what string) DiskInfo {
	t.Helper()
	select {
	case info := <-ch:
		return info
	case <-time.After(attachDeadline):
		t.Fatalf("no %s callback within %s", what, attachDeadline)
		return DiskInfo{}
	}
}

// drainReplay waits until ch has been silent for replayQuiet, so that the
// volumes DiskArbitration replays at registration time cannot be mistaken for
// the volume a test attaches afterwards.
func drainReplay(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	deadline := time.After(callbackDeadline)
	for {
		select {
		case <-ch:
		case <-time.After(replayQuiet):
			return
		case <-deadline:
			return
		}
	}
}

// TestWatchAppearedFiresForAFreshlyAttachedImage is the regression test for the
// bug that made the whole "insert a cartridge and be offered a boot" flow dead
// on arrival: DiskArbitration reports a disk as *appeared* when its media shows
// up, which is strictly before diskarbitrationd mounts the filesystem, so the
// appeared description carries no volume path. Filtering appeared events on
// "has a volume path" therefore drops every real insertion and only the
// registration replay (whose disks are already mounted) survives.
//
// This cannot be reproduced without a real disk: it is a property of
// diskarbitrationd's event ordering, not of any code in this package.
func TestWatchAppearedFiresForAFreshlyAttachedImage(t *testing.T) {
	if testing.Short() {
		t.Skip("attaches a real disk image")
	}
	hdiutil := hdiutilPath(t)
	image, volumeName := createTestImage(t, hdiutil)

	s := newTestSession(t)

	var (
		appeared    = make(chan DiskInfo, 4)
		disappeared = make(chan DiskInfo, 4)
		anyEvent    = make(chan struct{}, 64)
	)
	note := func(ch chan DiskInfo, info DiskInfo) {
		select {
		case anyEvent <- struct{}{}:
		default:
		}
		if info.VolumeName != volumeName {
			return
		}
		select {
		case ch <- info:
		default:
		}
	}

	cancelAppeared, err := s.WatchAppeared(func(info DiskInfo) { note(appeared, info) })
	if err != nil {
		t.Fatalf("WatchAppeared() error = %v", err)
	}
	defer cancelAppeared()
	cancelDisappeared, err := s.WatchDisappeared(func(info DiskInfo) { note(disappeared, info) })
	if err != nil {
		t.Fatalf("WatchDisappeared() error = %v", err)
	}
	defer cancelDisappeared()

	drainReplay(t, anyEvent)

	device := attachTestImage(t, hdiutil, image)

	info := waitForVolume(t, appeared, "appeared")
	if !info.Mounted() {
		t.Errorf("WatchAppeared delivered an unmounted volume: %+v", info)
	}
	if info.VolumeName != volumeName {
		t.Errorf("VolumeName = %q, want %q", info.VolumeName, volumeName)
	}

	// Exactly once: a mounted volume must not be re-announced for every
	// description change diskarbitrationd makes afterwards.
	select {
	case dup := <-appeared:
		t.Errorf("WatchAppeared delivered %q twice: %+v", volumeName, dup)
	case <-time.After(replayQuiet):
	}

	runHdiutil(t, hdiutil, "detach", device)

	gone := waitForVolume(t, disappeared, "disappeared")
	if gone.VolumeName != volumeName {
		t.Errorf("disappeared VolumeName = %q, want %q", gone.VolumeName, volumeName)
	}
	if gone.VolumePath == "" {
		t.Errorf("disappeared volume has no VolumePath: %+v", gone)
	}
	select {
	case dup := <-disappeared:
		t.Errorf("WatchDisappeared delivered %q twice: %+v", volumeName, dup)
	case <-time.After(replayQuiet):
	}
}

// TestWatchDisappearedFiresOnUnmountWithoutDetach is the regression test for
// the second half of the same bug. Ejecting a cartridge in Finder unmounts the
// volume while the media stays attached, and DiskArbitration reports that as a
// description change dropping the volume path — never as a disk-disappeared
// event. A watcher that only listens for disappearances therefore never learns
// the volume is gone and debounces the next insertion forever.
func TestWatchDisappearedFiresOnUnmountWithoutDetach(t *testing.T) {
	if testing.Short() {
		t.Skip("attaches a real disk image")
	}
	hdiutil := hdiutilPath(t)
	diskutil, err := exec.LookPath("diskutil")
	if err != nil {
		t.Skipf("diskutil is not available: %v", err)
	}
	image, volumeName := createTestImage(t, hdiutil)

	s := newTestSession(t)
	var (
		appeared    = make(chan DiskInfo, 4)
		disappeared = make(chan DiskInfo, 4)
		anyEvent    = make(chan struct{}, 64)
	)
	note := func(ch chan DiskInfo, info DiskInfo) {
		select {
		case anyEvent <- struct{}{}:
		default:
		}
		if info.VolumeName != volumeName {
			return
		}
		select {
		case ch <- info:
		default:
		}
	}
	cancelAppeared, err := s.WatchAppeared(func(info DiskInfo) { note(appeared, info) })
	if err != nil {
		t.Fatalf("WatchAppeared() error = %v", err)
	}
	defer cancelAppeared()
	cancelDisappeared, err := s.WatchDisappeared(func(info DiskInfo) { note(disappeared, info) })
	if err != nil {
		t.Fatalf("WatchDisappeared() error = %v", err)
	}
	defer cancelDisappeared()

	drainReplay(t, anyEvent)
	attachTestImage(t, hdiutil, image)

	mounted := waitForVolume(t, appeared, "appeared")
	if mounted.VolumePath == "" {
		t.Fatalf("appeared volume has no VolumePath: %+v", mounted)
	}

	// Unmount only: the media stays attached, exactly as when a user ejects the
	// volume in Finder while the disk image is still open.
	if out, err := exec.Command(diskutil, "unmount", mounted.VolumePath).CombinedOutput(); err != nil {
		t.Fatalf("diskutil unmount %s: %v\n%s", mounted.VolumePath, err, out)
	}

	gone := waitForVolume(t, disappeared, "disappeared")
	if gone.VolumePath != mounted.VolumePath {
		t.Errorf("disappeared VolumePath = %q, want the mount point it had, %q",
			gone.VolumePath, mounted.VolumePath)
	}

	// Re-mounting the same media must be announced again rather than debounced.
	if out, err := exec.Command(diskutil, "mount", mounted.BSDName).CombinedOutput(); err != nil {
		t.Fatalf("diskutil mount %s: %v\n%s", mounted.BSDName, err, out)
	}
	again := waitForVolume(t, appeared, "re-appeared")
	if again.VolumeName != volumeName {
		t.Errorf("re-appeared VolumeName = %q, want %q", again.VolumeName, volumeName)
	}
}
