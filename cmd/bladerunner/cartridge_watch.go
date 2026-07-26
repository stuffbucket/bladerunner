package main

// Mount detection — goal 4 of the standalone-cartridge design (design.md §8).
//
// A volume appears. Something has to decide, quickly and without touching
// volumes that are none of our business, whether it is a bladerunner cartridge
// worth offering to boot. That decision is this file, and it is deliberately
// PURE: decideForVolume takes a DiskArbitration description plus two lookups
// and returns a value. Every interesting case — an unrelated USB stick, a
// damaged cartridge, a cartridge this build is too old to boot, a cartridge
// that is already booted — is therefore testable with no real disk anywhere.
//
// The plumbing that owns a diskarb.Session and calls this lives in
// cartridge_watch_darwin.go; the consumers are `br watch` (watch.go) and the
// menubar.

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stuffbucket/bladerunner/internal/cartridge"
	"github.com/stuffbucket/bladerunner/internal/config"
	"github.com/stuffbucket/bladerunner/internal/diskarb"
	"github.com/stuffbucket/bladerunner/internal/instance"
	"github.com/stuffbucket/bladerunner/internal/logging"
)

// watchVerdict is what to do about one volume.
type watchVerdict string

const (
	// verdictIgnore means say nothing: the volume is not ours, or it is ours
	// and already booted. Reason is filled in for the log only.
	verdictIgnore watchVerdict = "ignore"
	// verdictWarn means the volume IS a cartridge but cannot be booted as it
	// stands. Reason says why and is meant to be shown: the user inserted
	// something that was supposed to boot, so silence would be the worst
	// possible answer.
	verdictWarn watchVerdict = "warn"
	// verdictOffer means the volume is a bootable cartridge nobody is holding.
	verdictOffer watchVerdict = "offer"

	// The verdicts below are never returned by decideForVolume; they report the
	// OUTCOME of an offer so `br watch --json` emits one record per event.
	verdictBooted   watchVerdict = "booted"
	verdictDeclined watchVerdict = "declined"
	verdictFailed   watchVerdict = "failed"
)

// watchAction is the decision (or outcome) for one volume, and the element of
// the `br watch --json` stream.
type watchAction struct {
	Verdict watchVerdict `json:"verdict"`
	// Name is the cartridge name, empty when the volume is not a cartridge.
	Name string `json:"name,omitempty"`
	// Volume is the user-visible volume name macOS gave the mount.
	Volume string `json:"volume,omitempty"`
	// Mountpoint is where the volume is mounted.
	Mountpoint string `json:"mountpoint,omitempty"`
	// DevNode is the BSD device backing the volume.
	DevNode string `json:"dev_node,omitempty"`
	// SourcePath is the .dmg/.sparseimage FILE behind the mount — the path a
	// holder must be given, because it converts the shipped artifact into its
	// own writable working copy rather than booting the read-only view.
	SourcePath string `json:"source_path,omitempty"`
	// HeldBy names the running instance already holding this volume.
	HeldBy string `json:"held_by,omitempty"`
	// Reason explains a warn or an ignore in words a user can act on.
	Reason string `json:"reason,omitempty"`
	// PID is the holder that was started (verdictBooted).
	PID int `json:"pid,omitempty"`
	// Error is why a boot failed (verdictFailed).
	Error string `json:"error,omitempty"`
}

// Reasons. They are constants so the log, the terminal and a notification all
// say the same thing about the same volume.
const (
	reasonNoVolume     = "carries no mounted volume"
	reasonNetwork      = "is a network volume"
	reasonNotCandidate = "is not a bladerunner cartridge volume"
	// reasonNoBackingImage is the honest refusal when the mounted view cannot
	// be traced back to the file behind it: booting the read-only view in place
	// is exactly what the cartridge design forbids.
	reasonNoBackingImage = "could not be traced back to a disk image file; boot it by path with 'br boot <file>'"
	// reasonUnreadable is the TCC failure mode, called out explicitly so a
	// permissions problem never reads as "no cartridge found". AirDropped
	// cartridges land in ~/Downloads and the menubar runs from a LaunchAgent
	// with no user-initiated open, so this is a routine outcome, not an
	// exotic one.
	reasonUnreadable = "could not be read (permission denied); grant Files and Folders access " +
		"for Downloads and removable volumes in System Settings > Privacy & Security"
)

// detectFunc is the authoritative "is this a cartridge" check, injected so the
// decision can be tested without a filesystem. It is cartridge.Detect.
type detectFunc func(volumePath string) (*cartridge.Detected, error)

// heldVolume is one volume offered to a held lookup. SourcePath is only known
// after Detect has traced the mount back to the file behind it, so the lookup
// is consulted twice: once cheaply, and once more with the source.
type heldVolume struct {
	// Mountpoint is where the volume is mounted.
	Mountpoint string
	// DevNode is the BSD device backing it.
	DevNode string
	// SourcePath is the .dmg/.sparseimage file behind the mount, when known.
	SourcePath string
}

// heldFunc reports the instance already holding a volume. A nil heldFunc means
// "nothing is held".
type heldFunc func(v heldVolume) (name string, held bool)

// decideForVolume classifies one appearing volume. It is pure apart from the
// two injected lookups, and it is the whole of the interesting logic.
//
// The order of the checks is the point:
//
//  1. cheap, name-only rejection first — this callback fires for every USB
//     stick, network share and disk image on the machine, and touching the
//     filesystem of an unrelated volume is both slow and rude;
//  2. then "do we already hold it?", because a running holder's own cartridge
//     mount appears exactly like a fresh insertion, and offering to boot a
//     cartridge that is already booted is a bug;
//  3. only then the authoritative Detect, which reads the volume — and asks
//     "do we already hold it?" a second time, now that the image file behind
//     the mount is known.
func decideForVolume(d diskarb.DiskInfo, detect detectFunc, held heldFunc) watchAction {
	a := watchAction{
		Verdict:    verdictIgnore,
		Volume:     d.VolumeName,
		Mountpoint: d.VolumePath,
		DevNode:    devNodePath(d.BSDName),
	}
	switch {
	case !d.Mounted():
		a.Reason = reasonNoVolume
		return a
	case d.NetworkVolume:
		a.Reason = reasonNetwork
		return a
	case !cartridge.IsCandidate(candidateName(d)):
		a.Reason = reasonNotCandidate
		return a
	}
	if name, ok := lookupHeld(held, heldVolume{Mountpoint: a.Mountpoint, DevNode: a.DevNode}); ok {
		return a.heldBy(name)
	}
	return decideDetected(a, detect, held)
}

// decideDetected runs the authoritative check on a volume that passed the
// name filter and turns the three-valued verdict into an action.
func decideDetected(a watchAction, detect detectFunc, held heldFunc) watchAction {
	det, err := detect(a.Mountpoint)
	if err != nil {
		return a.warn(fmt.Sprintf("could not be inspected: %v", err))
	}
	a.Name = det.Name
	if det.Mountpoint != "" {
		a.Mountpoint = det.Mountpoint
	}
	if det.DevNode != "" {
		a.DevNode = det.DevNode
	}

	switch det.Status {
	case cartridge.StatusNotCartridge:
		// A volume named like a cartridge that we could not read is the TCC
		// failure mode, not an ordinary negative: report it.
		if errors.Is(det.Err, fs.ErrPermission) {
			return a.warn(reasonUnreadable)
		}
		a.Reason = det.Reason
		return a
	case cartridge.StatusUnbootable:
		return a.warn(det.Reason)
	case cartridge.StatusBootable:
		// Handled below, where the remaining preconditions are checked.
	}

	// Bootable. Two things still have to hold: the name must be usable as an
	// instance name, and we must know the FILE behind the mount.
	if err := instance.ValidName(a.Name); err != nil {
		return a.warn(fmt.Sprintf("cannot be booted: %v", err))
	}
	if det.BackingImage == "" {
		return a.warn(reasonNoBackingImage)
	}
	a.SourcePath = det.BackingImage

	// Ask again now that the image is known. A SECOND mount of a cartridge we
	// are already running shares neither mountpoint nor device with the
	// holder's own — macOS calls it "/Volumes/bladerunner-demo 1" on a fresh
	// device — so the file behind it is the only thing that connects the two.
	if name, ok := lookupHeld(held, heldVolume{
		Mountpoint: a.Mountpoint,
		DevNode:    a.DevNode,
		SourcePath: a.SourcePath,
	}); ok {
		return a.heldBy(name)
	}

	a.Verdict = verdictOffer
	return a
}

// heldBy returns a copy of the action marked as a volume a live instance
// already owns: nothing to offer, and nothing to warn about.
func (a watchAction) heldBy(name string) watchAction {
	a.Verdict = verdictIgnore
	a.HeldBy = name
	if a.Name == "" {
		a.Name = name
	}
	a.Reason = fmt.Sprintf("is already running as instance %q", name)
	return a
}

// warn returns a copy of the action marked as a cartridge the user should be
// told about but cannot boot.
func (a watchAction) warn(reason string) watchAction {
	a.Verdict = verdictWarn
	a.Reason = reason
	return a
}

// outcome returns a copy of the action recording what became of an offer, so
// the accept path emits one further record instead of a second half-filled one.
func (a watchAction) outcome(v watchVerdict, pid int, err error) watchAction {
	a.Verdict = v
	a.PID = pid
	if err != nil {
		a.Error = err.Error()
	}
	return a
}

// describe renders an action as one human-readable line.
func (a watchAction) describe() string {
	label := a.Name
	if label == "" {
		label = a.Volume
	}
	switch a.Verdict {
	case verdictOffer:
		return fmt.Sprintf("cartridge %q on %s (from %s)", label, a.Mountpoint, a.SourcePath)
	case verdictWarn:
		return fmt.Sprintf("cartridge %q on %s %s", label, a.Mountpoint, a.Reason)
	case verdictBooted:
		return fmt.Sprintf("cartridge %q booting (holder pid %d)", label, a.PID)
	case verdictDeclined:
		return fmt.Sprintf("cartridge %q left alone", label)
	case verdictFailed:
		return fmt.Sprintf("cartridge %q could not be booted: %s", label, a.Error)
	default:
		return fmt.Sprintf("%s %s", a.Mountpoint, a.Reason)
	}
}

// candidateName is the string the cheap name filter is applied to: the volume
// name when DiskArbitration reported one, else the mount path (IsCandidate
// accepts either).
func candidateName(d diskarb.DiskInfo) string {
	if d.VolumeName != "" {
		return d.VolumeName
	}
	return d.VolumePath
}

// bsdDiskPrefix is the prefix of every BSD disk device name, and devDir is the
// directory the kernel exposes them in.
const (
	bsdDiskPrefix = "disk"
	devDir        = "/dev/"
)

// devNodePath renders a BSD name ("disk4s1") as the device path other parts of
// the tree record ("/dev/disk4s1"). An empty or already-absolute name is
// returned unchanged.
func devNodePath(bsdName string) string {
	if bsdName == "" || strings.HasPrefix(bsdName, "/") {
		return bsdName
	}
	return devDir + bsdName
}

// wholeDiskUnit reduces a device node to its whole-disk unit: "/dev/disk4s1",
// "disk4s1s2" and "/dev/rdisk4" all reduce to "disk4". Anything that does not
// look like a BSD disk device reduces to "".
//
// Matching on the unit rather than the exact node is what makes the
// already-running check work: a holder records the whole disk it attached
// ("/dev/disk4") while DiskArbitration reports the slice that carries the
// filesystem ("disk4s1").
func wholeDiskUnit(devNode string) string {
	name := strings.TrimPrefix(filepath.Base(strings.TrimSpace(devNode)), "r")
	if !strings.HasPrefix(name, bsdDiskPrefix) {
		return ""
	}
	i := len(bsdDiskPrefix)
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == len(bsdDiskPrefix) {
		return ""
	}
	return name[:i]
}

// lookupHeld consults a possibly-nil heldFunc.
func lookupHeld(held heldFunc, v heldVolume) (string, bool) {
	if held == nil {
		return "", false
	}
	return held(v)
}

// heldVolumes indexes the volumes currently held by a live instance, so the
// watcher can ignore the mount a holder made for itself — and the second mount
// a user made of the same file.
//
// Three keys, because each is the only one available in some real case:
//
//   - the SOURCE IMAGE, which is the only thing shared by a holder's mount and
//     an independent Finder mount of the same .dmg (different mountpoint,
//     different device, same file). Both the source and the working copy are
//     indexed, since the two spellings name one cartridge;
//   - the DEVICE, matched on the whole-disk unit, because a holder records the
//     disk it attached while DiskArbitration reports the slice;
//   - the MOUNTPOINT, for an instance written by an older holder that carries
//     only its state dir.
func heldVolumes(root string) heldFunc {
	entries, err := instance.List(root)
	if err != nil {
		logging.L().Debug("list instance registry for cartridge watch", "err", err)
	}
	mounts := make(map[string]string, len(entries))
	devs := make(map[string]string, len(entries))
	sources := make(map[string]string, len(entries))
	for i := range entries {
		e := &entries[i]
		if !instance.Alive(*e) {
			continue
		}
		addPathKeys(mounts, e.Mountpoint, e.Name)
		if e.Kind == instance.KindCartridge {
			// A cartridge instance is rooted AT its mountpoint.
			addPathKeys(mounts, e.StateDir, e.Name)
		}
		if unit := wholeDiskUnit(e.DevNode); unit != "" {
			devs[unit] = e.Name
		}
		for _, image := range []string{e.SourcePath, e.WorkingCopy} {
			if key := cartridgeImageKey(image); key != "" {
				sources[key] = e.Name
			}
		}
	}
	return func(v heldVolume) (string, bool) {
		if key := cartridgeImageKey(v.SourcePath); key != "" {
			if name, ok := sources[key]; ok {
				return name, true
			}
		}
		if unit := wholeDiskUnit(v.DevNode); unit != "" {
			if name, ok := devs[unit]; ok {
				return name, true
			}
		}
		for _, k := range pathKeys(v.Mountpoint) {
			if name, ok := mounts[k]; ok {
				return name, true
			}
		}
		return "", false
	}
}

// pathKeys returns the forms a mountpoint may be spelled in: the cleaned path
// and, when it differs, its symlink-resolved form (/Volumes/x vs /private/...).
func pathKeys(path string) []string {
	if path == "" {
		return nil
	}
	clean := filepath.Clean(path)
	keys := []string{clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil && resolved != clean {
		keys = append(keys, resolved)
	}
	return keys
}

// addPathKeys records every spelling of path as belonging to name.
func addPathKeys(m map[string]string, path, name string) {
	for _, k := range pathKeys(path) {
		m[k] = name
	}
}

// --- the watcher ----------------------------------------------------------

// cartridgeWatcher turns a stream of appearing volumes into at most one action
// per volume. It is the shared body of `br watch` and the menubar's watcher;
// only the session wiring differs.
//
// Concurrency: observe is called from the DiskArbitration serial queue, and the
// startup catch-up sweep runs on the caller's goroutine. Both go through the
// same mutex-guarded seen map, which is what makes a cartridge that is present
// at startup AND reported by the appeared stream produce exactly one offer.
//
// The sink must not block: it runs on the DA queue, where a multi-second
// blocking prompt would stall every other callback on the session.
type cartridgeWatcher struct {
	detect detectFunc
	// held is re-evaluated per volume rather than captured once, so a holder
	// that started a moment ago is already visible.
	held func() heldFunc
	sink func(watchAction)

	mu   sync.Mutex
	seen map[string]bool
}

// newCartridgeWatcher builds a watcher against the real cartridge detector and
// the instance registry under root.
func newCartridgeWatcher(root string, sink func(watchAction)) *cartridgeWatcher {
	return &cartridgeWatcher{
		detect: cartridge.Detect,
		held:   func() heldFunc { return heldVolumes(root) },
		sink:   sink,
		seen:   make(map[string]bool),
	}
}

// observe decides about one volume and delivers the action at most once.
func (w *cartridgeWatcher) observe(d diskarb.DiskInfo) {
	key := volumeKey(d)
	if w.handled(key) {
		// Already decided about this volume. Returning here — rather than after
		// deciding again — is what keeps a repeated event from re-reading the
		// volume, not just from re-prompting.
		return
	}
	action := decideForVolume(d, w.detect, w.heldNow())
	if action.Verdict == verdictIgnore {
		// Nothing was shown and nothing is remembered: an ignore costs one
		// name comparison, and remembering every volume on the machine would
		// grow without bound over the life of a menubar session.
		logging.L().Debug("cartridge watch ignored a volume",
			"volume", action.Volume, "mountpoint", action.Mountpoint, "reason", action.Reason)
		return
	}
	if !w.claim(key) {
		return // a concurrent observe of the same volume won the race
	}
	w.sink(action)
}

// forget drops the memory of a volume that went away, so re-inserting the same
// cartridge offers again.
func (w *cartridgeWatcher) forget(d diskarb.DiskInfo) {
	key := volumeKey(d)
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.seen, key)
}

// catchUp feeds the volumes that were already mounted before the watcher
// started. A cartridge inserted before the menubar launched is the common case
// (AirDrop auto-mounts it), so skipping this would miss it entirely.
func (w *cartridgeWatcher) catchUp(disks []diskarb.DiskInfo) {
	for i := range disks {
		w.observe(disks[i])
	}
}

// heldNow snapshots the currently held volumes for one decision.
func (w *cartridgeWatcher) heldNow() heldFunc {
	if w.held == nil {
		return nil
	}
	return w.held()
}

// claim records a volume as handled and reports whether this call is the one
// that claimed it.
func (w *cartridgeWatcher) claim(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[key] {
		return false
	}
	w.seen[key] = true
	return true
}

// handled reports whether a volume has already been decided about.
func (w *cartridgeWatcher) handled(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seen[key]
}

// volumeKey identifies a volume across the appeared stream, the catch-up sweep
// and the disappeared stream. The whole-disk unit is preferred because the same
// mount can be reported for the whole disk and for its slice; the mountpoint is
// the fallback for a disk with no usable BSD name.
func volumeKey(d diskarb.DiskInfo) string {
	if unit := wholeDiskUnit(d.BSDName); unit != "" {
		return unit
	}
	if d.VolumePath != "" {
		return filepath.Clean(d.VolumePath)
	}
	return d.VolumeName
}

// --- the accept path ------------------------------------------------------

// errNoCartridgeSource is returned when an offer somehow reached the boot path
// without the file behind the mount.
var errNoCartridgeSource = errors.New("no cartridge image file to boot")

// bootDetectedCartridge boots an offered cartridge and returns the PID of the
// holder now owning it.
//
// The read-only view is unmounted FIRST, and that is the whole subtlety of this
// function. A shipped cartridge is a compressed read-only .dmg: the holder
// re-opens the SOURCE file and converts it into its own writable working copy,
// so the Finder mount is not what gets booted. Leaving it attached would strand
// a second mount of the same image on the desktop and hand the user an eject
// gesture that no longer drains anything.
//
// Because that unmount is destructive to the user's view of the volume, the
// "is it already running?" question is asked BEFORE it — by image, not by
// mountpoint, since the mount this offer came from is by definition not the
// holder's. A spawn that fails afterwards says so plainly, including that the
// volume is now unmounted; claiming success would be worse than the failure.
func bootDetectedCartridge(a watchAction) (int, error) {
	if a.SourcePath == "" {
		return 0, errNoCartridgeSource
	}
	root := config.DefaultStateDir()
	if err := ensureCartridgeBootable(a.SourcePath, a.Name); err != nil {
		return 0, err
	}
	if a.Mountpoint != "" {
		if err := cartridge.Detach(a.Mountpoint); err != nil {
			return 0, fmt.Errorf("unmount %s: %w", a.Mountpoint, err)
		}
	}
	pid, err := spawnHolder(holderSpawn{StateDir: root, Name: a.Name, CartridgePath: a.SourcePath})
	if err != nil {
		return 0, fmt.Errorf("boot cartridge %q: %w (its volume was unmounted first; retry with 'br boot %s')",
			a.Name, err, a.SourcePath)
	}
	return pid, nil
}
