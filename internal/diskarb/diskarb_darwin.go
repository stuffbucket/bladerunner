//go:build darwin

package diskarb //nolint:gocritic // dupImport false-positives on the cgo `import "C"` clause

/*
#cgo LDFLAGS: -framework DiskArbitration -framework CoreFoundation
#include <stdlib.h>
#include <dispatch/dispatch.h>
#include <CoreFoundation/CoreFoundation.h>
#include <DiskArbitration/DiskArbitration.h>

// Forward declarations of the Go callbacks below. This preamble deliberately
// contains declarations only: cgo copies it into a second generated C file for
// the //export machinery, so any definition here would be emitted twice.
//
// Note also that this file is compiled as plain C, not Objective-C. The repo's
// AppKit bridges use "-x objective-c -fobjc-arc", but DiskArbitration,
// CoreFoundation and libdispatch are pure C APIs, and ARC would not manage
// CoreFoundation objects anyway (every CFRelease below would still be manual).
// Compiling as C also keeps dispatch_queue_t a plain struct pointer and keeps
// dispatch_release available, both of which ARC takes away.
extern void brDiskarbAppeared(DADiskRef disk, void *ctx);
extern void brDiskarbDisappeared(DADiskRef disk, void *ctx);
extern void brDiskarbChanged(DADiskRef disk, CFArrayRef keys, void *ctx);
extern DADissenterRef brDiskarbUnmountApproval(DADiskRef disk, void *ctx);
extern void brDiskarbBarrier(void *ctx);
*/
import "C" //nolint:gocritic // dupImport false-positives on the cgo `import "C"` clause

import (
	"bytes"
	"errors"
	"fmt"
	"runtime/cgo"
	"sync"
	"unsafe" //nolint:gocritic // dupImport false-positives on the cgo `import "C"` clause

	"golang.org/x/sys/unix"
)

// queueLabel is the label of the private serial queue every session creates.
// It shows up in spindumps and Instruments, so it is worth being specific.
const queueLabel = "com.stuffbucket.bladerunner.diskarb"

// dissentStatus is the DAReturn reported to whoever asked for the unmount when
// a watcher denies it. kDAReturnBusy is what a "the volume is in use" veto
// looks like to Finder, diskutil and hdiutil, and is the only status that makes
// them report something a user can act on rather than a generic failure.
const dissentStatus = C.kDAReturnBusy

// DiskArbitration description keys, resolved once from the framework's CFString
// constants into Go strings. Descriptions are decoded by walking the dictionary
// and comparing key names (see diskInfoFromDisk) rather than by calling
// CFDictionaryGetValue: CoreFoundation object references are typed as uintptr
// by cgo, and handing one to CFDictionaryGetValue's "const void *" parameter
// would require a uintptr -> unsafe.Pointer conversion that go vet rightly
// rejects.
var (
	keyMediaBSDName   = cfStringToGo(C.kDADiskDescriptionMediaBSDNameKey)
	keyVolumeName     = cfStringToGo(C.kDADiskDescriptionVolumeNameKey)
	keyVolumePath     = cfStringToGo(C.kDADiskDescriptionVolumePathKey)
	keyVolumeKind     = cfStringToGo(C.kDADiskDescriptionVolumeKindKey)
	keyMediaEjectable = cfStringToGo(C.kDADiskDescriptionMediaEjectableKey)
	keyMediaRemovable = cfStringToGo(C.kDADiskDescriptionMediaRemovableKey)
	keyMediaWhole     = cfStringToGo(C.kDADiskDescriptionMediaWholeKey)
	keyVolumeNetwork  = cfStringToGo(C.kDADiskDescriptionVolumeNetworkKey)
	keyDeviceModel    = cfStringToGo(C.kDADiskDescriptionDeviceModelKey)
)

// watcherKind selects which DiskArbitration callbacks a watcher is registered
// for, and therefore which C trampolines were handed to the framework.
type watcherKind int

const (
	kindAppeared watcherKind = iota
	kindDisappeared
	kindUnmountApproval
)

// watchesVolumes reports whether this kind tracks mounted volumes, and so
// registers the appeared/disappeared/description-changed trio rather than the
// single unmount-approval callback.
func (k watcherKind) watchesVolumes() bool {
	return k == kindAppeared || k == kindDisappeared
}

// callbackPtrs returns every C function pointer registered for this kind. The
// same pointers must be passed back to DAUnregisterCallback, which matches
// registrations on the (callback, context) pair.
func (k watcherKind) callbackPtrs() []unsafe.Pointer {
	if k.watchesVolumes() {
		return []unsafe.Pointer{
			unsafe.Pointer((*[0]byte)(C.brDiskarbAppeared)),
			unsafe.Pointer((*[0]byte)(C.brDiskarbDisappeared)),
			unsafe.Pointer((*[0]byte)(C.brDiskarbChanged)),
		}
	}
	if k == kindUnmountApproval {
		return []unsafe.Pointer{unsafe.Pointer((*[0]byte)(C.brDiskarbUnmountApproval))}
	}
	return nil
}

// Session owns a DASessionRef and the private serial dispatch queue its
// callbacks are delivered on. A Session is safe for concurrent use.
type Session struct {
	mu       sync.Mutex
	closed   bool
	session  C.DASessionRef
	queue    C.dispatch_queue_t
	watchers map[*watcher]struct{}
}

// watcher is one registered DiskArbitration callback.
//
// The Go closure is reached from C through a cgo.Handle whose value is stored
// in a malloc'd uintptr_t used as the DiskArbitration context pointer. The
// indirection buys two things: the framework gets a genuine, per-registration C
// pointer (which is what DAUnregisterCallback matches on), and the code never
// has to convert a uintptr back into an unsafe.Pointer.
type watcher struct {
	sess   *Session
	kind   watcherKind
	ctx    unsafe.Pointer // malloc'd uintptr_t holding handle
	handle cgo.Handle

	bsdFilter string

	mu        sync.Mutex
	canceled  bool
	onDisk    func(DiskInfo)
	onApprove func(DiskInfo) Dissent
	// volumes folds the raw event stream into mount transitions; nil for the
	// unmount-approval kind, which is stateless.
	volumes *volumeTracker

	cancelOnce sync.Once
}

// NewSession creates a DiskArbitration session bound to a fresh private serial
// dispatch queue. Close it when done.
func NewSession() (*Session, error) {
	session := C.DASessionCreate(C.kCFAllocatorDefault)
	if session == 0 {
		return nil, errors.New("diskarb: DASessionCreate failed")
	}

	label := C.CString(queueLabel)
	// A NULL attr is DISPATCH_QUEUE_SERIAL: callbacks are delivered one at a
	// time, which is what lets Close and CancelFunc use a queue barrier to know
	// no callback is in flight.
	queue := C.dispatch_queue_create(label, nil)
	C.free(unsafe.Pointer(label))
	if queue == nil {
		C.CFRelease(C.CFTypeRef(session))
		return nil, errors.New("diskarb: dispatch_queue_create failed")
	}
	// Tag the queue with itself so onQueue can recognize "already running on my
	// own callback queue" and skip the barrier that would deadlock there.
	C.dispatch_queue_set_specific(queue, unsafe.Pointer(queue), unsafe.Pointer(queue), nil)
	C.DASessionSetDispatchQueue(session, queue)

	return &Session{
		session:  session,
		queue:    queue,
		watchers: make(map[*watcher]struct{}),
	}, nil
}

// Close unregisters every watcher, detaches the session from its queue and
// releases both. It is safe to call more than once and safe to call from inside
// a callback.
//
// Teardown order matters and is the same as CancelFunc's (see cancel):
// mark canceled, DAUnregisterCallback, drain the queue, only then delete the
// cgo.Handles. Deleting a handle before the drain would let a callback that is
// already running on the queue dereference a dead handle and take the process
// down. As in cancel, the session lock is released before the drain so that a
// callback canceling itself cannot deadlock against it.
func (s *Session) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	watchers := make([]*watcher, 0, len(s.watchers))
	for w := range s.watchers {
		watchers = append(watchers, w)
	}
	s.watchers = nil
	session, queue := s.session, s.queue
	s.session, s.queue = 0, nil
	s.mu.Unlock()

	for _, w := range watchers {
		w.markCanceled()
		for _, ptr := range w.kind.callbackPtrs() {
			C.DAUnregisterCallback(session, ptr, w.ctx)
		}
	}
	// Stop delivery entirely, then wait for anything already dispatched.
	C.DASessionSetDispatchQueue(session, nil)
	barrier(queue)

	for _, w := range watchers {
		w.handle.Delete()
		C.free(w.ctx)
	}

	C.CFRelease(C.CFTypeRef(session))
	releaseQueue(queue)
	return nil
}

// WatchAppeared calls fn once for every volume that becomes mounted.
//
// "Appeared" here means "a mounted volume became available", not "a device node
// showed up". DiskArbitration reports media, and fires its disk-appeared
// callback before diskarbitrationd has mounted anything, so an appeared
// description for a freshly attached disk carries no volume path at all; the
// mount point turns up milliseconds later as a description change. This package
// therefore watches disk-appeared, disk-disappeared and changes to
// kDADiskDescriptionVolumePathKey together, and delivers the first event that
// finds a volume mounted — exactly once per mount, whether it came from the
// insertion or from the replay described below.
//
// DiskArbitration also replays the disks that are already present at
// registration time, so a caller that only cares about "is my cartridge here"
// usually does not need CurrentDisks as well. Media with no mounted filesystem
// (bare disks, APFS containers) is never delivered.
func (s *Session) WatchAppeared(fn func(DiskInfo)) (CancelFunc, error) {
	return s.registerDisk(kindAppeared, fn)
}

// WatchDisappeared calls fn once for every volume that stops being mounted.
//
// It covers both ways a volume can go away: an unmount that leaves the media
// attached (which DiskArbitration reports as a description change dropping the
// volume path, not as a disappearance) and the media itself vanishing. Only
// volumes that were seen mounted are reported, and each is reported once.
//
// The description handed to fn is the one recorded while the volume was
// mounted, so VolumePath still reports where it used to be.
func (s *Session) WatchDisappeared(fn func(DiskInfo)) (CancelFunc, error) {
	return s.registerDisk(kindDisappeared, fn)
}

// WatchUnmountApproval asks to be consulted before a volume is unmounted.
//
// An empty bsdName watches every disk; otherwise only disks on the same
// whole-disk unit are delivered (so "disk4" also sees "disk4s1").
//
// fn runs on the session's serial queue and MUST return promptly: whoever asked
// for the unmount is blocked until it does, and a slow answer is
// indistinguishable from a hung Finder. It must not wait for a VM to shut down.
// The intended shape is to return Deny("...") immediately, start the drain on a
// goroutine, and let the next unmount attempt (or an explicit unmount once the
// drain is finished) through.
func (s *Session) WatchUnmountApproval(bsdName string, fn func(DiskInfo) Dissent) (CancelFunc, error) {
	if fn == nil {
		return nil, ErrNilCallback
	}
	return s.register(kindUnmountApproval, bsdName, nil, fn)
}

// registerDisk is the shared body of WatchAppeared and WatchDisappeared.
func (s *Session) registerDisk(kind watcherKind, fn func(DiskInfo)) (CancelFunc, error) {
	if fn == nil {
		return nil, ErrNilCallback
	}
	return s.register(kind, "", fn, nil)
}

// register creates the watcher, publishes it through a cgo.Handle and hands the
// matching C trampolines to DiskArbitration.
//
// A volume-watching kind registers three callbacks against the same context
// pointer: disk-appeared and disk-disappeared for the media, and
// description-changed for the mount and unmount that happen in between. All
// three feed the watcher's volumeTracker, which decides what the caller
// actually sees.
func (s *Session) register(kind watcherKind, bsdFilter string, onDisk func(DiskInfo), onApprove func(DiskInfo) Dissent) (CancelFunc, error) {
	w := &watcher{
		sess:      s,
		kind:      kind,
		bsdFilter: bsdFilter,
		onDisk:    onDisk,
		onApprove: onApprove,
	}
	if kind.watchesVolumes() {
		w.volumes = newVolumeTracker()
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSessionClosed
	}

	ctx := C.malloc(C.size_t(unsafe.Sizeof(C.uintptr_t(0))))
	if ctx == nil {
		s.mu.Unlock()
		return nil, errors.New("diskarb: out of memory allocating callback context")
	}
	w.handle = cgo.NewHandle(w)
	w.ctx = ctx
	*(*C.uintptr_t)(ctx) = C.uintptr_t(w.handle)
	s.watchers[w] = struct{}{}

	if kind.watchesVolumes() {
		C.DARegisterDiskAppearedCallback(s.session, 0, C.DADiskAppearedCallback((*[0]byte)(C.brDiskarbAppeared)), ctx)
		C.DARegisterDiskDisappearedCallback(s.session, 0, C.DADiskDisappearedCallback((*[0]byte)(C.brDiskarbDisappeared)), ctx)
		watchKeys := volumePathWatchKeys()
		C.DARegisterDiskDescriptionChangedCallback(s.session, 0, watchKeys,
			C.DADiskDescriptionChangedCallback((*[0]byte)(C.brDiskarbChanged)), ctx)
		// DiskArbitration retains the key array for the life of the
		// registration, so this drops our own reference rather than the
		// framework's.
		if watchKeys != 0 {
			C.CFRelease(C.CFTypeRef(watchKeys))
		}
	}
	if kind == kindUnmountApproval {
		C.DARegisterDiskUnmountApprovalCallback(s.session, 0, C.DADiskUnmountApprovalCallback((*[0]byte)(C.brDiskarbUnmountApproval)), ctx)
	}
	s.mu.Unlock()

	return func() { w.cancel() }, nil
}

// volumePathWatchKeys builds the +1 CFArray of description keys the
// description-changed callback is filtered on: just the volume path, because
// that is the key whose arrival and departure mean "mounted" and "unmounted".
//
// The single element is staged through a malloc'd cell rather than a Go array
// so that a CFStringRef — which cgo models as an integer — never has to be
// converted into an unsafe.Pointer, which go vet rightly rejects. This is the
// same trick register uses to hand a cgo.Handle to C.
//
// A zero return means "could not build the filter"; the caller then registers
// for every description change instead, which is noisier but still correct
// because the volumeTracker discards changes that alter nothing it cares about.
func volumePathWatchKeys() C.CFArrayRef {
	cell := C.malloc(C.size_t(unsafe.Sizeof(C.uintptr_t(0))))
	if cell == nil {
		return 0
	}
	defer C.free(cell)
	*(*C.uintptr_t)(cell) = C.uintptr_t(C.kDADiskDescriptionVolumePathKey)
	// CFArrayCreate copies the element out of cell, so freeing it is safe.
	// kCFTypeArrayCallBacks makes the array retain the key, matching what
	// DiskArbitration expects of the filter it is handed.
	callbacks := &C.kCFTypeArrayCallBacks
	return C.CFArrayCreate(C.kCFAllocatorDefault, (*unsafe.Pointer)(cell), 1, callbacks)
}

// cancel unregisters one watcher.
//
// Ordering, which is the whole point of this function:
//
//  1. mark the watcher canceled, so a callback that starts running now returns
//     without touching the caller's closure;
//  2. DAUnregisterCallback, so the framework stops dispatching new ones;
//  3. drain the serial queue with a synchronous barrier, so a callback that was
//     *already* running has returned;
//  4. only now delete the cgo.Handle and free the context.
//
// Doing (4) before (3) is the classic crash in this API: DAUnregisterCallback
// makes no promise about callbacks already handed to the queue.
//
// Steps (1) and (2) run under the session lock; (3) and (4) deliberately do
// not. Holding any lock a callback might also want across the barrier deadlocks
// the moment a callback cancels itself: the barrier waits for the queue, the
// queue waits for the lock. Because the queue reference is retained for the
// duration of the barrier, a concurrent Close cannot pull it out from under it.
func (w *watcher) cancel() {
	w.cancelOnce.Do(func() {
		s := w.sess
		s.mu.Lock()
		if s.closed {
			// Close already unregistered, drained and freed this watcher.
			s.mu.Unlock()
			return
		}
		delete(s.watchers, w)
		w.markCanceled()
		for _, ptr := range w.kind.callbackPtrs() {
			C.DAUnregisterCallback(s.session, ptr, w.ctx)
		}
		queue := s.queue
		retainQueue(queue)
		s.mu.Unlock()

		barrier(queue)
		releaseQueue(queue)

		w.handle.Delete()
		C.free(w.ctx)
	})
}

// markCanceled stops any further delivery into Go for this watcher and drops
// the volumes it was remembering.
func (w *watcher) markCanceled() {
	w.mu.Lock()
	w.canceled = true
	if w.volumes != nil {
		w.volumes.forget()
	}
	w.mu.Unlock()
}

// observe folds one event into the watcher's view of which volumes are mounted
// and returns the callback to run together with the description to hand it, or
// a nil callback when this event changes nothing the caller asked about.
//
// The tracker is consulted for every kind of volume event, including the ones
// this watcher does not report: a WatchAppeared watcher still has to see
// unmounts so a re-inserted cartridge is announced again, and a WatchDisappeared
// watcher still has to see mounts so it knows there is anything to report.
//
// The closure is copied out under the lock and invoked without it, so user code
// never runs while a lock is held.
func (w *watcher) observe(info DiskInfo, mountedNow bool) (func(DiskInfo), DiskInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.canceled || w.volumes == nil {
		return nil, DiskInfo{}
	}
	state, out := w.volumes.observe(info, mountedNow)
	switch state {
	case mountAppeared:
		if w.kind != kindAppeared {
			return nil, DiskInfo{}
		}
	case mountVanished:
		if w.kind != kindDisappeared {
			return nil, DiskInfo{}
		}
	case mountUnchanged:
		return nil, DiskInfo{}
	}
	return w.onDisk, out
}

// approvalFunc returns the approval callback, or nil once canceled.
func (w *watcher) approvalFunc() func(DiskInfo) Dissent {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.canceled {
		return nil
	}
	return w.onApprove
}

// watcherFromContext recovers the watcher a DiskArbitration callback belongs
// to. The context is the malloc'd uintptr_t written by register; the ordering
// in cancel/Close guarantees it is still alive whenever C calls in.
func watcherFromContext(ctx unsafe.Pointer) *watcher {
	if ctx == nil {
		return nil
	}
	w, ok := cgo.Handle(*(*C.uintptr_t)(ctx)).Value().(*watcher)
	if !ok {
		return nil
	}
	return w
}

//export brDiskarbAppeared
func brDiskarbAppeared(disk C.DADiskRef, ctx unsafe.Pointer) {
	// The media is here; whether a filesystem is mounted on it is whatever the
	// description says, which for a fresh attachment is "not yet".
	deliverDisk(disk, ctx, false)
}

//export brDiskarbDisappeared
func brDiskarbDisappeared(disk C.DADiskRef, ctx unsafe.Pointer) {
	// The media is gone, so nothing is mounted on it any more, whatever the
	// description DiskArbitration retained still claims.
	deliverDisk(disk, ctx, true)
}

//export brDiskarbChanged
func brDiskarbChanged(disk C.DADiskRef, _ C.CFArrayRef, ctx unsafe.Pointer) {
	// The volume path was added or removed: this is the callback that actually
	// reports mounts and unmounts. The changed keys are ignored because the
	// registration already filters on kDADiskDescriptionVolumePathKey, and the
	// volumeTracker discards anything that changes nothing.
	deliverDisk(disk, ctx, false)
}

// deliverDisk is the shared body of the appeared/disappeared/changed
// trampolines. It turns one raw DiskArbitration event into at most one call
// into the watcher's Go callback; see watcher.observe for which events survive.
//
// mediaGone marks the events that mean the device itself has vanished, whose
// retained description still names the mount point the volume used to have.
func deliverDisk(disk C.DADiskRef, ctx unsafe.Pointer, mediaGone bool) {
	w := watcherFromContext(ctx)
	if w == nil {
		return
	}
	info, ok := diskInfoFromDisk(disk)
	if !ok {
		return
	}
	if !MatchesFilter(w.bsdFilter, info.BSDName) {
		return
	}
	fn, out := w.observe(info, info.Mounted() && !mediaGone)
	if fn == nil {
		return
	}
	fn(out)
}

//export brDiskarbUnmountApproval
func brDiskarbUnmountApproval(disk C.DADiskRef, ctx unsafe.Pointer) C.DADissenterRef {
	w := watcherFromContext(ctx)
	if w == nil {
		return nil
	}
	fn := w.approvalFunc()
	if fn == nil {
		return nil
	}
	info, ok := diskInfoFromDisk(disk)
	if !ok || !MatchesFilter(w.bsdFilter, info.BSDName) {
		return nil
	}
	d := fn(info)
	if !d.Deny {
		return nil
	}
	return newDissenter(d.Reason)
}

//export brDiskarbBarrier
func brDiskarbBarrier(_ unsafe.Pointer) {}

// newDissenter builds the +1 DADissenterRef returned to DiskArbitration. The
// framework takes ownership of the returned reference, so it is deliberately
// not released here; the CFString built for the reason is ours and is.
func newDissenter(reason string) C.DADissenterRef {
	if reason == "" {
		return C.DADissenterCreate(C.kCFAllocatorDefault, dissentStatus, 0)
	}
	creason := C.CString(reason)
	cfReason := C.CFStringCreateWithCString(C.kCFAllocatorDefault, creason, C.kCFStringEncodingUTF8)
	C.free(unsafe.Pointer(creason))
	d := C.DADissenterCreate(C.kCFAllocatorDefault, dissentStatus, cfReason)
	if cfReason != 0 {
		C.CFRelease(C.CFTypeRef(cfReason))
	}
	return d
}

// barrier blocks until everything already queued on the session's serial queue
// has finished. Called after DAUnregisterCallback, it is what makes "no
// callback is in flight" true.
//
// When the caller is already running on that queue — a callback canceling
// itself, or closing the session — a synchronous dispatch would deadlock. In
// that case there is nothing to wait for anyway: the queue is serial, so the
// caller *is* the only callback running.
func barrier(queue C.dispatch_queue_t) {
	if queue == nil || onQueue(queue) {
		return
	}
	C.dispatch_sync_f(queue, nil, C.dispatch_function_t((*[0]byte)(C.brDiskarbBarrier)))
}

// onQueue reports whether the calling thread is already executing on queue.
func onQueue(queue C.dispatch_queue_t) bool {
	return C.dispatch_get_specific(unsafe.Pointer(queue)) != nil
}

// releaseQueue drops one reference to the dispatch queue.
//
// dispatch_release takes the transparent union dispatch_object_t, which cgo
// models as an opaque byte array; writing the queue pointer into its first
// member is the only way to express the union from Go. It is not a pointer
// laundering trick: the destination is an ordinary local, and the value stored
// is a C pointer the Go collector never tracks.
func releaseQueue(queue C.dispatch_queue_t) {
	if queue == nil {
		return
	}
	C.dispatch_release(queueObject(queue))
}

// retainQueue takes a reference to the dispatch queue, so that a barrier can
// outlive a concurrent Close.
func retainQueue(queue C.dispatch_queue_t) {
	if queue == nil {
		return
	}
	C.dispatch_retain(queueObject(queue))
}

// queueObject wraps a queue in the dispatch_object_t union. See releaseQueue.
func queueObject(queue C.dispatch_queue_t) C.dispatch_object_t {
	var obj C.dispatch_object_t
	*(*C.dispatch_queue_t)(unsafe.Pointer(&obj)) = queue
	return obj
}

// CurrentDisks enumerates every currently mounted volume, so a watcher can
// catch up on cartridges that were mounted before it started.
//
// It walks the kernel's mounted filesystem table (getfsstat) and asks
// DiskArbitration to describe each mount point; entries DiskArbitration does
// not recognize as disks (devfs, autofs, and friends) are skipped.
func (s *Session) CurrentDisks() ([]DiskInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrSessionClosed
	}

	paths, err := mountedVolumePaths()
	if err != nil {
		return nil, err
	}
	disks := make([]DiskInfo, 0, len(paths))
	for _, path := range paths {
		info, ok := s.describeVolumePath(path)
		if !ok || !info.Mounted() {
			continue
		}
		disks = append(disks, info)
	}
	return disks, nil
}

// CurrentDisks enumerates every currently mounted volume using a short-lived
// private session. Callers that already hold a Session should prefer the
// method, which avoids creating and tearing down a session per call.
func CurrentDisks() ([]DiskInfo, error) {
	s, err := NewSession()
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	return s.CurrentDisks()
}

// describeVolumePath resolves one mount point to its DiskArbitration
// description. Callers must hold s.mu.
func (s *Session) describeVolumePath(path string) (DiskInfo, bool) {
	cpath := C.CString(path)
	url := C.CFURLCreateFromFileSystemRepresentation(
		C.kCFAllocatorDefault,
		(*C.UInt8)(unsafe.Pointer(cpath)),
		C.CFIndex(len(path)),
		C.true,
	)
	C.free(unsafe.Pointer(cpath))
	if url == 0 {
		return DiskInfo{}, false
	}
	disk := C.DADiskCreateFromVolumePath(C.kCFAllocatorDefault, s.session, url)
	C.CFRelease(C.CFTypeRef(url))
	if disk == 0 {
		return DiskInfo{}, false
	}
	info, ok := diskInfoFromDisk(disk)
	C.CFRelease(C.CFTypeRef(disk))
	return info, ok
}

// mountedVolumePaths returns the mount point of every mounted filesystem.
func mountedVolumePaths() ([]string, error) {
	count, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, fmt.Errorf("diskarb: counting mounted filesystems: %w", err)
	}
	if count <= 0 {
		return nil, nil
	}
	buf := make([]unix.Statfs_t, count)
	count, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, fmt.Errorf("diskarb: listing mounted filesystems: %w", err)
	}
	if count > len(buf) {
		count = len(buf)
	}
	paths := make([]string, 0, count)
	for i := range buf[:count] {
		if path := unix.ByteSliceToString(buf[i].Mntonname[:]); path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// diskInfoFromDisk copies a disk's DiskArbitration description into a DiskInfo.
//
// DADiskCopyDescription returns a +1 dictionary that this function owns and
// releases; every value read out of it is a borrowed reference and must not be.
func diskInfoFromDisk(disk C.DADiskRef) (DiskInfo, bool) {
	if disk == 0 {
		return DiskInfo{}, false
	}
	desc := C.DADiskCopyDescription(disk)
	if desc == 0 {
		return DiskInfo{}, false
	}
	defer C.CFRelease(C.CFTypeRef(desc))

	count := int(C.CFDictionaryGetCount(desc))
	if count <= 0 {
		return DiskInfo{}, false
	}
	keys := make([]unsafe.Pointer, count)
	values := make([]unsafe.Pointer, count)
	C.CFDictionaryGetKeysAndValues(desc, &keys[0], &values[0])

	var info DiskInfo
	for i := range count {
		switch cfStringToGo(C.CFStringRef(uintptr(keys[i]))) {
		case keyMediaBSDName:
			info.BSDName = cfValueString(values[i])
		case keyVolumeName:
			info.VolumeName = cfValueString(values[i])
		case keyVolumePath:
			info.VolumePath = cfValueURLPath(values[i])
		case keyVolumeKind:
			info.VolumeKind = cfValueString(values[i])
		case keyDeviceModel:
			info.DeviceModel = cfValueString(values[i])
		case keyMediaEjectable:
			info.Ejectable = cfValueBool(values[i])
		case keyMediaRemovable:
			info.Removable = cfValueBool(values[i])
		case keyMediaWhole:
			info.WholeDisk = cfValueBool(values[i])
		case keyVolumeNetwork:
			info.NetworkVolume = cfValueBool(values[i])
		}
	}
	return info, true
}

// cfValueString reads a borrowed dictionary value as a string, or "" if it is
// absent or of another type.
func cfValueString(value unsafe.Pointer) string {
	ref := C.CFTypeRef(uintptr(value))
	if ref == 0 || C.CFGetTypeID(ref) != C.CFStringGetTypeID() {
		return ""
	}
	return cfStringToGo(C.CFStringRef(ref))
}

// cfValueBool reads a borrowed dictionary value as a boolean, or false if it is
// absent or of another type.
func cfValueBool(value unsafe.Pointer) bool {
	ref := C.CFTypeRef(uintptr(value))
	if ref == 0 || C.CFGetTypeID(ref) != C.CFBooleanGetTypeID() {
		return false
	}
	return C.CFBooleanGetValue(C.CFBooleanRef(ref)) != C.false
}

// cfValueURLPath reads a borrowed dictionary value as a file URL and returns
// its POSIX path.
func cfValueURLPath(value unsafe.Pointer) string {
	ref := C.CFTypeRef(uintptr(value))
	if ref == 0 || C.CFGetTypeID(ref) != C.CFURLGetTypeID() {
		return ""
	}
	// CFURLCopyFileSystemPath returns +1.
	path := C.CFURLCopyFileSystemPath(C.CFURLRef(ref), C.kCFURLPOSIXPathStyle)
	if path == 0 {
		return ""
	}
	defer C.CFRelease(C.CFTypeRef(path))
	return cfStringToGo(path)
}

// cfStringToGo converts a CFStringRef to a Go string, returning "" when the
// reference is NULL or cannot be rendered as UTF-8.
func cfStringToGo(ref C.CFStringRef) string {
	if ref == 0 {
		return ""
	}
	// CFStringGetLength counts UTF-16 units; ask CoreFoundation how many bytes
	// those could need in UTF-8 rather than guessing, and add the NUL.
	size := C.CFStringGetMaximumSizeForEncoding(C.CFStringGetLength(ref), C.kCFStringEncodingUTF8)
	if size <= 0 {
		return ""
	}
	buf := make([]byte, int(size)+1)
	ok := C.CFStringGetCString(ref, (*C.char)(unsafe.Pointer(&buf[0])), C.CFIndex(len(buf)), C.kCFStringEncodingUTF8)
	if ok == C.false {
		return ""
	}
	// CFStringGetCString NUL-terminates; the buffer was sized for the worst
	// case, so trim at the terminator. Cut returns the whole slice when the
	// terminator is somehow absent.
	text, _, _ := bytes.Cut(buf, []byte{0})
	return string(text)
}
